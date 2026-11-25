// Package localci ties the pieces together: resolve the commit, load its config, build the
// sandbox and providers, run the steps and report.
package localci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PrashantsCodeRepo/LocalCI/internal/config"
	"github.com/PrashantsCodeRepo/LocalCI/internal/gitx"
	"github.com/PrashantsCodeRepo/LocalCI/internal/provider"
	"github.com/PrashantsCodeRepo/LocalCI/internal/report"
	"github.com/PrashantsCodeRepo/LocalCI/internal/runner"
)

// Options for one run.
type Options struct {
	Dir       string // any directory inside the repository
	Rev       string // revision to check; default HEAD
	Tier      string // "" or "full"
	Out       io.Writer
	CacheRoot string // default ~/.cache/localci (or $XDG_CACHE_HOME/localci)
}

// Outcome of a run.
type Outcome struct {
	Repo    string
	SHA     string
	Config  *config.Config
	Results []runner.Result
	Passed  bool
	Elapsed time.Duration
	LogPath string
}

// DefaultCacheRoot is where deps and run logs live.
func DefaultCacheRoot() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "localci")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "localci")
}

// Resolve finds the repository and commit for opts.
func Resolve(ctx context.Context, opts Options) (repo, sha string, err error) {
	dir := opts.Dir
	if dir == "" {
		dir = "."
	}
	repo, err = gitx.Toplevel(ctx, dir)
	if err != nil {
		return "", "", err
	}
	rev := opts.Rev
	if rev == "" {
		rev = "HEAD"
	}
	sha, err = gitx.ResolveCommit(ctx, repo, rev)
	return repo, sha, err
}

// Run checks one commit. It returns an error only when the checks could not be run at all;
// failing steps are reported in Outcome.Passed. Every resource it creates is released
// before it returns, including when ctx is cancelled by a signal.
func Run(ctx context.Context, opts Options) (out *Outcome, err error) {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.CacheRoot == "" {
		opts.CacheRoot = DefaultCacheRoot()
	}
	start := time.Now()
	repo, sha, err := Resolve(ctx, opts)
	if err != nil {
		return nil, err
	}
	out = &Outcome{Repo: repo, SHA: sha}

	// Each commit is judged by its own checks.
	data, ok, err := gitx.ShowFile(ctx, repo, sha, config.FileName)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, fmt.Errorf("%s has no %s", sha[:12], config.FileName)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return out, err
	}
	out.Config = cfg

	log, err := report.OpenLog(filepath.Join(opts.CacheRoot, "runs"), sha, opts.Out)
	if err != nil {
		return out, err
	}
	defer log.Close()
	out.LogPath = log.Path
	w := log.Writer
	defer func() {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(w, "localci: interrupted; cleaned up")
		} else if err != nil {
			fmt.Fprintf(w, "localci: %v\n", err)
		}
	}()
	fmt.Fprintf(w, "localci: checking %s (%s) in a clean worktree\n", sha[:12], repo)

	for _, r := range cfg.Requires {
		if _, err := exec.LookPath(r); err != nil {
			return out, fmt.Errorf("required command %q is not on PATH", r)
		}
	}

	sb, err := gitx.NewSandbox(ctx, repo, sha)
	if err != nil {
		return out, err
	}
	defer sb.Close()

	var pathDirs, extraEnv []string
	node := "node"
	if v, ok := cfg.Toolchains["node"]; ok {
		bin, err := provider.ResolveToolchain("node", v)
		if err != nil {
			return out, err
		}
		node = bin
		// Steps and npm's `#!/usr/bin/env node` call it "node". Put a per-run shim first on
		// PATH so that name is the validated binary even when it is installed as node22 or
		// shares a directory with another node.
		shim, err := os.MkdirTemp("", "localci-bin-")
		if err != nil {
			return out, err
		}
		defer os.RemoveAll(shim)
		if err := os.Symlink(bin, filepath.Join(shim, "node")); err != nil {
			return out, err
		}
		pathDirs = append(pathDirs, shim, filepath.Dir(bin))
		fmt.Fprintf(w, "localci: node %s -> %s\n", v, bin)
	} else if p, err := exec.LookPath("node"); err == nil {
		node = p
	}

	extraEnv = append(extraEnv, "CI=true", "LOCALCI=1", "LOCALCI_SHA="+sha, "LOCALCI_WORKTREE="+sb.Dir)
	env := func() []string { return buildEnv(pathDirs, extraEnv) }

	// Python deps first: a fresh pip install of a local path writes build files into the
	// worktree, which is cleaned before node_modules links are added.
	cache := &provider.DepCache{Root: filepath.Join(opts.CacheRoot, "deps"), Out: w}
	cleaned := false
	for _, kind := range []string{"python", "npm"} {
		for _, d := range cfg.Deps {
			if d.Kind != kind {
				continue
			}
			if kind == "npm" && !cleaned {
				if err := sb.Clean(ctx); err != nil {
					return out, err
				}
				cleaned = true
			}
			p, err := cache.Prepare(ctx, cfg.Project, d, sb.Dir, env(), node)
			if err != nil {
				return out, err
			}
			defer p.Release()
			pathDirs = append(p.PathDirs, pathDirs...)
			extraEnv = append(extraEnv, p.Env...)
		}
	}
	if !cleaned {
		if err := sb.Clean(ctx); err != nil {
			return out, err
		}
	}

	if cfg.Postgres != nil {
		pg := provider.NewPostgres(cfg.Project, *cfg.Postgres)
		defer func() {
			if terr := pg.Teardown(); terr != nil {
				fmt.Fprintf(w, "localci: warning: %v\n", terr)
			}
		}()
		if err := pg.Setup(ctx, cfg.Project); err != nil {
			return out, err
		}
		u, err := pg.URL(pg.Name)
		if err != nil {
			return out, err
		}
		extraEnv = append(extraEnv, cfg.Postgres.Env+"="+u)
		fmt.Fprintf(w, "localci: postgres database %s (%s)\n", pg.Name, cfg.Postgres.Env)
	}

	// Config env last, expanded against everything above.
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		base := env()
		extraEnv = append(extraEnv, k+"="+os.Expand(cfg.Env[k], lookup(base)))
	}

	r := &runner.Runner{Root: sb.Dir, Env: env(), Out: w, FailFast: cfg.FailFastOn(), Tier: opts.Tier}
	out.Results = r.Run(ctx, cfg.Groups)
	out.Passed = runner.Summary(w, out.Results) && ctx.Err() == nil
	out.Elapsed = time.Since(start).Round(time.Second)
	verdict := "PASSED"
	if !out.Passed {
		verdict = "FAILED"
	}
	fmt.Fprintf(w, "\nlocalci: %s %s in %s (log: %s)\n", sha[:12], verdict, out.Elapsed, log.Path)
	return out, ctx.Err()
}

// buildEnv is this process's environment with pathDirs ahead of PATH and extra appended
// (later entries win when a step's shell reads them).
func buildEnv(pathDirs, extra []string) []string {
	var env []string
	path := os.Getenv("PATH")
	skip := map[string]bool{"PATH": true}
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		skip[k] = true
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !skip[k] {
			env = append(env, kv)
		}
	}
	if len(pathDirs) > 0 {
		path = strings.Join(pathDirs, string(os.PathListSeparator)) + string(os.PathListSeparator) + path
	}
	env = append(env, "PATH="+path)
	// Keep only the last value of each extra key.
	last := map[string]int{}
	for i, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		last[k] = i
	}
	for i, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		if last[k] == i {
			env = append(env, kv)
		}
	}
	return env
}

func lookup(env []string) func(string) string {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return func(k string) string { return m[k] }
}

// Statuses builds the GitHub statuses for an outcome.
func Statuses(o *Outcome, runErr error) []report.Status {
	ctxName := "localci"
	if o.Config != nil {
		ctxName = o.Config.StatusContext()
	}
	suffix := " (self-reported)"
	if runErr != nil && !o.Passed && o.Results == nil {
		return []report.Status{{State: "error", Context: ctxName, Description: "could not run: " + runErr.Error()}}
	}
	total, passed := 0, 0
	var firstFail string
	groups := map[string][]runner.Result{}
	var order []string
	for _, r := range o.Results {
		if _, ok := groups[r.Group]; !ok {
			order = append(order, r.Group)
		}
		groups[r.Group] = append(groups[r.Group], r)
		if r.Outcome == runner.Skipped {
			continue
		}
		total++
		if !r.Failed() && r.Outcome != runner.NotRun {
			passed++
		}
		if r.Failed() && firstFail == "" {
			firstFail = r.Group + "/" + r.Step
		}
	}
	var out []report.Status
	if o.Passed {
		out = append(out, report.Status{State: "success", Context: ctxName,
			Description: fmt.Sprintf("%d/%d steps passed in %s%s", passed, total, o.Elapsed, suffix)})
	} else {
		d := "failed"
		if firstFail != "" {
			d = firstFail + " failed"
		}
		out = append(out, report.Status{State: "failure", Context: ctxName, Description: d + suffix})
	}
	if o.Config != nil && o.Config.Status.PerGroup {
		for _, g := range order {
			st, desc := "success", "passed"
			for _, r := range groups[g] {
				if r.Failed() {
					st, desc = "failure", r.Step+" "+string(r.Outcome)
					break
				}
				if r.Outcome == runner.NotRun {
					st, desc = "error", "not run"
				}
			}
			out = append(out, report.Status{State: st, Context: ctxName + "/" + g, Description: desc + suffix})
		}
	}
	return out
}

// Post sends every status for the outcome.
func Post(ctx context.Context, o *Outcome, runErr error, w io.Writer) error {
	for _, s := range Statuses(o, runErr) {
		if err := report.PostStatus(ctx, o.Repo, o.SHA, s); err != nil {
			return err
		}
		fmt.Fprintf(w, "localci: posted %s=%s on %s\n", s.Context, s.State, o.SHA[:12])
	}
	return nil
}

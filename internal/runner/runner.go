// Package runner executes the configured steps.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PrashantsCodeRepo/LocalCI/internal/config"
)

// Outcome of one step.
type Outcome string

const (
	Pass      Outcome = "pass"
	Fail      Outcome = "fail"
	InfoFail  Outcome = "fail (informational)"
	Skipped   Outcome = "skipped"
	NotRun    Outcome = "not run"
	TimedOut  Outcome = "timed out"
	Cancelled Outcome = "cancelled"
)

// Result of one step.
type Result struct {
	Group, Step string
	Outcome     Outcome
	Elapsed     time.Duration
	Note        string
}

// Failed reports whether the result fails the run.
func (r Result) Failed() bool {
	return r.Outcome == Fail || r.Outcome == TimedOut || r.Outcome == Cancelled
}

// Runner holds what every step shares.
type Runner struct {
	Root     string   // worktree root; step cwd is relative to it
	Env      []string // base environment, KEY=VALUE
	Out      io.Writer
	FailFast bool
	Tier     string // "" or "full"
}

// Run executes the groups in order and returns every step's result, including those not run.
func (r *Runner) Run(ctx context.Context, groups []config.Group) []Result {
	var results []Result
	stop := false
	for _, g := range groups {
		if stop || ctx.Err() != nil {
			for _, s := range g.Steps {
				results = append(results, Result{Group: g.Name, Step: s.Name, Outcome: NotRun})
			}
			continue
		}
		fmt.Fprintf(r.Out, "\n=== group %s%s\n", g.Name, map[bool]string{true: " (parallel)"}[g.Parallel])
		var rs []Result
		if g.Parallel {
			rs = r.runParallel(ctx, g)
		} else {
			rs = r.runSerial(ctx, g)
		}
		for _, x := range rs {
			if x.Failed() && r.FailFast {
				stop = true
			}
		}
		results = append(results, rs...)
	}
	return results
}

func (r *Runner) runSerial(ctx context.Context, g config.Group) []Result {
	var out []Result
	stop := false
	for _, s := range g.Steps {
		if stop || ctx.Err() != nil {
			out = append(out, Result{Group: g.Name, Step: s.Name, Outcome: NotRun})
			continue
		}
		res := r.runStep(ctx, g.Name, s, r.Out)
		out = append(out, res)
		if res.Failed() && r.FailFast {
			stop = true
		}
	}
	return out
}

// runParallel buffers each step's output and prints it whole when the step ends, so logs
// do not interleave.
func (r *Runner) runParallel(ctx context.Context, g config.Group) []Result {
	out := make([]Result, len(g.Steps))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, s := range g.Steps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			res := r.runStep(ctx, g.Name, s, &buf)
			mu.Lock()
			r.Out.Write(buf.Bytes())
			mu.Unlock()
			out[i] = res
		}()
	}
	wg.Wait()
	return out
}

func (r *Runner) runStep(ctx context.Context, group string, s config.Step, w io.Writer) Result {
	res := Result{Group: group, Step: s.Name}
	if s.Tier == "full" && r.Tier != "full" {
		res.Outcome = Skipped
		res.Note = "tier full (use --tier full)"
		fmt.Fprintf(w, "\n--- %s: skipped, %s\n", s.Name, res.Note)
		return res
	}
	fmt.Fprintf(w, "\n--- %s\n", s.Name)

	timeout := time.Duration(s.Timeout)
	if timeout <= 0 {
		timeout = config.DefaultTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := append([]string{}, r.Env...)
	lookup := envLookup(env)
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+os.Expand(s.Env[k], lookup))
	}

	cmd := exec.CommandContext(sctx, "bash", "-eo", "pipefail", "-c", s.Run)
	cmd.Dir = filepath.Join(r.Root, s.Cwd)
	cmd.Env = env
	cmd.Stdin = nil // /dev/null: a step that reads stdin must not hang or eat the hook's input
	cmd.Stdout, cmd.Stderr = w, w
	// Own process group, so a timeout or Ctrl-C kills the whole tree, not just bash.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	err := cmd.Run()
	// Children that outlived bash would keep the group alive; kill it either way.
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	res.Elapsed = time.Since(start).Round(100 * time.Millisecond)

	switch {
	case err == nil:
		res.Outcome = Pass
	case errors.Is(sctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.Outcome = TimedOut
		res.Note = "after " + timeout.String()
	case ctx.Err() != nil:
		res.Outcome = Cancelled
	case s.Informational:
		res.Outcome = InfoFail
		res.Note = err.Error()
	default:
		res.Outcome = Fail
		res.Note = err.Error()
	}
	if s.Informational && res.Outcome == TimedOut {
		res.Outcome = InfoFail
	}
	fmt.Fprintf(w, "--- %s: %s (%s)\n", s.Name, res.Outcome, res.Elapsed)
	return res
}

func envLookup(env []string) func(string) string {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return func(k string) string { return m[k] }
}

// Summary writes a result table and returns whether the run passed.
func Summary(w io.Writer, results []Result) bool {
	ok := true
	fmt.Fprintln(w, "\n=== summary")
	for _, r := range results {
		if r.Failed() {
			ok = false
		}
		line := fmt.Sprintf("  %-22s %-36s %-20s", r.Group, r.Step, r.Outcome)
		if r.Elapsed > 0 {
			line += " " + r.Elapsed.String()
		}
		if r.Note != "" && r.Outcome != Fail {
			line += "  " + r.Note
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	return ok
}

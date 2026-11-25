package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/PrashantsCodeRepo/LocalCI/internal/config"
)

const completeMarker = ".complete"

// keyRE matches the cache key part of an install directory name (see hashParts).
var keyRE = regexp.MustCompile(`^[0-9a-f]{20}$`)

// DepCache keeps dependency installs under Root, one directory per cache key.
type DepCache struct {
	Root string
	Out  io.Writer
}

// Prepared is what a dependency adds to the step environment.
type Prepared struct {
	PathDirs  []string // prepended to PATH
	Env       []string // KEY=VALUE
	Installed bool     // a fresh install ran (it may have left files in the worktree)
	release   func()
}

// Release drops this run's in-use marker.
func (p *Prepared) Release() {
	if p != nil && p.release != nil {
		p.release()
	}
}

// PidAlive reports whether a process exists (kill -0).
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func hashParts(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:", len(p))
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}

// Ensure makes sure dir holds a completed install, running install into it if not, and
// marks it in use by this process. Installs happen in place (venvs cannot be moved) under
// an exclusive lock, so concurrent runs with the same key wait instead of racing. A failed
// install removes the directory.
func Ensure(dir string, install func(dir string) error) (installed bool, release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return false, nil, err
	}
	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err != nil {
		if err := os.RemoveAll(dir); err != nil {
			return false, nil, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, nil, err
		}
		if err := install(dir); err != nil {
			os.RemoveAll(dir)
			return false, nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, completeMarker), nil, 0o644); err != nil {
			os.RemoveAll(dir)
			return false, nil, err
		}
		installed = true
	}
	// Mark in use while still holding the lock, so a pruner cannot slip in between.
	marker := filepath.Join(dir, ".inuse."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return installed, nil, err
	}
	now := time.Now()
	os.Chtimes(dir, now, now)
	return installed, func() { os.Remove(marker) }, nil
}

// Prune keeps the keep most recently used installs named prefix-*, and never removes one
// that a live process has marked in use or that is being installed right now.
func Prune(root, prefix string, keep int) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cs []cand
	for _, e := range entries {
		key, ok := strings.CutPrefix(e.Name(), prefix+"-")
		if !e.IsDir() || !ok || !keyRE.MatchString(key) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cs = append(cs, cand{filepath.Join(root, e.Name()), info.ModTime()})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].mod.After(cs[j].mod) })
	if len(cs) <= keep {
		return nil
	}
	for _, c := range cs[keep:] {
		if inUse(c.path) {
			continue
		}
		lock, err := os.OpenFile(c.path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			continue
		}
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			if !inUse(c.path) {
				os.RemoveAll(c.path)
				os.Remove(c.path + ".lock")
			}
			syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
		lock.Close()
	}
	return nil
}

func inUse(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, ".inuse.*"))
	for _, m := range matches {
		pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Ext(m), "."))
		if err == nil && PidAlive(pid) {
			return true
		}
	}
	return false
}

// Prepare installs (or reuses) one dependency for the worktree at tree. env is the step
// environment so installs use the same toolchain as the tests; node is the node binary.
func (c *DepCache) Prepare(ctx context.Context, project string, d config.Dep, tree string, env []string, node string) (*Prepared, error) {
	root := filepath.Join(c.Root, project)
	switch d.Kind {
	case "npm":
		return c.prepareNpm(ctx, root, d, tree, env, node)
	case "python":
		return c.preparePython(ctx, root, d, tree, env)
	}
	return nil, fmt.Errorf("deps %s: unknown kind %q", d.Name, d.Kind)
}

func (c *DepCache) prepareNpm(ctx context.Context, root string, d config.Dep, tree string, env []string, node string) (*Prepared, error) {
	src := filepath.Join(tree, d.Dir)
	manifest, err := os.ReadFile(filepath.Join(src, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("deps %s: %w", d.Name, err)
	}
	lockfile, err := os.ReadFile(filepath.Join(src, "package-lock.json"))
	if err != nil {
		return nil, fmt.Errorf("deps %s: %w (npm ci needs a lockfile)", d.Name, err)
	}
	ver, err := exec.CommandContext(ctx, node, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("deps %s: %s --version: %w", d.Name, node, err)
	}
	// Both files are in the key: when they disagree npm ci fails, as a clean deploy build
	// would, while a key on the lockfile alone would reuse an old install and pass.
	key := hashParts([]byte("npm"), ver, manifest, lockfile)
	dir := filepath.Join(root, d.Name+"-"+key)
	installed, release, err := Ensure(dir, func(dir string) error {
		fmt.Fprintf(c.Out, "localci: installing %s (npm ci, node %s), cached for next time\n", d.Name, strings.TrimSpace(string(ver)))
		if err := os.WriteFile(filepath.Join(dir, "package.json"), manifest, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), lockfile, 0o644); err != nil {
			return err
		}
		return runInstall(ctx, c.Out, dir, env, "npm", "ci", "--no-audit", "--no-fund", "--loglevel=error")
	})
	if err != nil {
		return nil, fmt.Errorf("deps %s: %w", d.Name, err)
	}
	link := filepath.Join(src, "node_modules")
	if _, err := os.Lstat(link); err == nil {
		release()
		return nil, fmt.Errorf("deps %s: %s already exists in the commit", d.Name, link)
	}
	if err := os.Symlink(filepath.Join(dir, "node_modules"), link); err != nil {
		release()
		return nil, err
	}
	Prune(root, d.Name, d.Keep)
	return &Prepared{Installed: installed, release: release}, nil
}

func (c *DepCache) preparePython(ctx context.Context, root string, d config.Dep, tree string, env []string) (*Prepared, error) {
	py, err := ResolveToolchain("python", d.Python)
	if err != nil {
		return nil, fmt.Errorf("deps %s: %w", d.Name, err)
	}
	ver, err := exec.CommandContext(ctx, py, "--version").Output()
	if err != nil {
		return nil, err
	}
	parts := [][]byte{[]byte("python"), ver, []byte(strings.Join(d.Install, "\x00"))}
	for _, m := range d.Manifests {
		b, err := os.ReadFile(filepath.Join(tree, m))
		if err != nil {
			return nil, fmt.Errorf("deps %s: %w", d.Name, err)
		}
		parts = append(parts, []byte(m), b)
	}
	key := hashParts(parts...)
	dir := filepath.Join(root, d.Name+"-"+key)
	venv := filepath.Join(dir, "venv")
	installed, release, err := Ensure(dir, func(dir string) error {
		fmt.Fprintf(c.Out, "localci: installing %s (%s, pip install %s), cached for next time\n", d.Name, strings.TrimSpace(string(ver)), strings.Join(d.Install, " "))
		if err := runInstall(ctx, c.Out, tree, env, py, "-m", "venv", venv); err != nil {
			return err
		}
		args := append([]string{"-m", "pip", "install", "--quiet", "--disable-pip-version-check"}, d.Install...)
		return runInstall(ctx, c.Out, tree, env, filepath.Join(venv, "bin", "python"), args...)
	})
	if err != nil {
		return nil, fmt.Errorf("deps %s: %w", d.Name, err)
	}
	Prune(root, d.Name, d.Keep)
	return &Prepared{
		PathDirs:  []string{filepath.Join(venv, "bin")},
		Env:       []string{"VIRTUAL_ENV=" + venv},
		Installed: installed,
		release:   release,
	}, nil
}

// LookPathIn finds name on the PATH in env (exec.Command would search this process's PATH).
func LookPathIn(name string, env []string) string {
	if strings.Contains(name, "/") {
		return name
	}
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, d := range filepath.SplitList(path) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return name
}

func runInstall(ctx context.Context, out io.Writer, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, LookPathIn(name, env), args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

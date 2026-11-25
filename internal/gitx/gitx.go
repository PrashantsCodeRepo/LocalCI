// Package gitx wraps the git commands LocalCI needs.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Run runs git in dir and returns trimmed stdout.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ClearLocalEnv unsets every variable `git rev-parse --local-env-vars` lists. Git exports
// GIT_DIR and others to hooks; left set, they point git commands that tests run in their
// own temp repos back at this repo.
func ClearLocalEnv() error {
	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse --local-env-vars: %w", err)
	}
	for _, v := range strings.Fields(string(out)) {
		os.Unsetenv(v)
	}
	return nil
}

// Toplevel returns the root of the working tree that contains dir.
func Toplevel(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--show-toplevel")
}

// ResolveCommit turns a revision into a full commit SHA.
func ResolveCommit(ctx context.Context, repo, rev string) (string, error) {
	return Run(ctx, repo, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
}

// ShowFile returns a file's contents at a commit, and false if the commit lacks it.
func ShowFile(ctx context.Context, repo, sha, path string) ([]byte, bool, error) {
	if _, err := Run(ctx, repo, "cat-file", "-e", sha+":"+path); err != nil {
		return nil, false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "show", sha+":"+path)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return nil, false, fmt.Errorf("git show %s:%s: %w", sha, path, err)
	}
	return out, true, nil
}

// Sandbox is a throwaway detached worktree at one commit.
type Sandbox struct {
	Repo string // the repository it was added from
	Dir  string // the worktree root
	tmp  string
}

// NewSandbox adds a detached worktree for sha in a new temp directory.
func NewSandbox(ctx context.Context, repo, sha string) (*Sandbox, error) {
	tmp, err := os.MkdirTemp("", "localci-")
	if err != nil {
		return nil, err
	}
	// Resolve symlinks (/var -> /private/var on macOS) so paths match what tools report.
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	s := &Sandbox{Repo: repo, Dir: filepath.Join(tmp, "tree"), tmp: tmp}
	if _, err := Run(ctx, repo, "worktree", "add", "--quiet", "--detach", s.Dir, sha); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	return s, nil
}

// Clean removes untracked and ignored files, restoring the pristine checkout.
func (s *Sandbox) Clean(ctx context.Context) error {
	_, err := Run(ctx, s.Dir, "clean", "-ffdxq")
	return err
}

// Close removes the worktree and its temp directory. It is safe to call more than once and
// uses a fresh context so it still works after the run's context is cancelled.
func (s *Sandbox) Close() error {
	if s == nil || s.tmp == "" {
		return nil
	}
	ctx := context.Background()
	_, err := Run(ctx, s.Repo, "worktree", "remove", "--force", s.Dir)
	if rerr := os.RemoveAll(s.tmp); rerr != nil && err == nil {
		err = rerr
	}
	// If remove failed (e.g. the dir was already gone), prune the stale registration.
	Run(ctx, s.Repo, "worktree", "prune")
	s.tmp = ""
	return err
}

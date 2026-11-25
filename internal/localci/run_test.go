package localci

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PrashantsCodeRepo/LocalCI/internal/runner"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir string, files map[string]string) string {
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "c")
	return gitIn(t, dir, "rev-parse", "HEAD")
}

const passing = `project: demo
env:
  WT: ${LOCALCI_WORKTREE}
groups:
  - name: checks
    steps:
      - name: file is the committed one
        run: grep -qx committed a.txt
      - name: env
        run: '[ "$WT" = "$PWD" ] && [ "$CI" = true ] && [ -n "$LOCALCI_SHA" ]'
`

func TestRunChecksTheCommitNotTheWorkingTree(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	sha := commit(t, repo, map[string]string{".localci.yml": passing, "a.txt": "committed\n"})
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("dirty\n"), 0o644)

	var buf bytes.Buffer
	cache := t.TempDir()
	o, err := Run(context.Background(), Options{Dir: repo, Out: &buf, CacheRoot: cache})
	if err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if !o.Passed || o.SHA != sha {
		t.Fatalf("passed=%v sha=%s\n%s", o.Passed, o.SHA, buf.String())
	}
	if log, _ := os.ReadFile(filepath.Join(cache, "runs", sha+".log")); !bytes.Contains(log, []byte("PASSED")) {
		t.Errorf("log missing or incomplete:\n%s", log)
	}
	if wt := gitIn(t, repo, "worktree", "list"); strings.Count(wt, "\n") != 0 {
		t.Errorf("sandbox worktree left behind:\n%s", wt)
	}
}

func TestEachCommitUsesItsOwnConfig(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	good := commit(t, repo, map[string]string{".localci.yml": passing, "a.txt": "committed\n"})
	commit(t, repo, map[string]string{".localci.yml": strings.Replace(passing, "grep -qx committed", "grep -qx nope", 1)})

	o, err := Run(context.Background(), Options{Dir: repo, Out: &bytes.Buffer{}, CacheRoot: t.TempDir()})
	if err != nil || o.Passed {
		t.Fatalf("HEAD should fail: passed=%v err=%v", o.Passed, err)
	}
	o, err = Run(context.Background(), Options{Dir: repo, Rev: good, Out: &bytes.Buffer{}, CacheRoot: t.TempDir()})
	if err != nil || !o.Passed {
		t.Fatalf("older commit should pass: passed=%v err=%v", o.Passed, err)
	}
}

func TestRunWithoutConfigFails(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	commit(t, repo, map[string]string{"a.txt": "x"})
	if _, err := Run(context.Background(), Options{Dir: repo, Out: &bytes.Buffer{}, CacheRoot: t.TempDir()}); err == nil || !strings.Contains(err.Error(), ".localci.yml") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingRequiredCommandFails(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	commit(t, repo, map[string]string{".localci.yml": "project: demo\nrequires: [no-such-tool-xyz]\n" + passing[len("project: demo\n"):]})
	_, err := Run(context.Background(), Options{Dir: repo, Out: &bytes.Buffer{}, CacheRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no-such-tool-xyz") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatuses(t *testing.T) {
	o := &Outcome{Passed: false, Results: []runner.Result{
		{Group: "a", Step: "x", Outcome: runner.Pass},
		{Group: "b", Step: "y", Outcome: runner.Fail},
	}}
	s := Statuses(o, nil)
	if len(s) != 1 || s[0].State != "failure" || s[0].Context != "localci" || !strings.Contains(s[0].Description, "b/y failed") {
		t.Errorf("%+v", s)
	}
}

func TestHookInstallRespectsHooksPathAndExistingHooks(t *testing.T) {
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "config", "core.hooksPath", ".githooks")
	path, err := InstallHook(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(repo, ".githooks", "pre-push"); path != want {
		t.Errorf("path %s, want %s", path, want)
	}
	// Reinstalling our own hook is fine; replacing someone else's needs --force.
	if _, err := InstallHook(context.Background(), repo, false); err != nil {
		t.Errorf("reinstall: %v", err)
	}
	os.WriteFile(path, []byte("#!/bin/sh\necho mine\n"), 0o755)
	if _, err := InstallHook(context.Background(), repo, false); err == nil {
		t.Error("replaced a foreign hook without --force")
	}
	if _, err := InstallHook(context.Background(), repo, true); err != nil {
		t.Errorf("--force: %v", err)
	}
}

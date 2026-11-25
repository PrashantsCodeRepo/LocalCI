package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo makes a repo with one commit holding the given files and returns its path and SHA.
func newRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q", "-b", "main")
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-qm", "init")
	sha, err := ResolveCommit(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return dir, sha
}

func TestSandboxChecksOutCommitIgnoringWorkingTree(t *testing.T) {
	ctx := context.Background()
	repo, sha := newRepo(t, map[string]string{"a.txt": "committed\n"})
	// An uncommitted edit must not reach the sandbox.
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("dirty\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("x"), 0o644)

	sb, err := NewSandbox(ctx, repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(sb.Dir, "a.txt"))
	if string(got) != "committed\n" {
		t.Errorf("a.txt = %q, want committed content", got)
	}
	if _, err := os.Stat(filepath.Join(sb.Dir, "untracked.txt")); err == nil {
		t.Error("untracked file leaked into the sandbox")
	}
	head, _ := Run(ctx, sb.Dir, "rev-parse", "HEAD")
	if head != sha {
		t.Errorf("sandbox HEAD %s, want %s", head, sha)
	}

	dir := sb.Dir
	if err := sb.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("sandbox dir still exists after Close: %v", err)
	}
	list, _ := Run(ctx, repo, "worktree", "list", "--porcelain")
	if want := "worktree " + dir; strings.Contains(list, want) {
		t.Errorf("worktree still registered:\n%s", list)
	}
	if err := sb.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestSandboxCleanRemovesUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	repo, sha := newRepo(t, map[string]string{"a.txt": "a"})
	sb, err := NewSandbox(ctx, repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	os.MkdirAll(filepath.Join(sb.Dir, "build"), 0o755)
	os.WriteFile(filepath.Join(sb.Dir, "build", "x"), nil, 0o644)
	if err := sb.Clean(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sb.Dir, "build")); !os.IsNotExist(err) {
		t.Error("Clean left build/ behind")
	}
}

func TestShowFile(t *testing.T) {
	ctx := context.Background()
	repo, sha := newRepo(t, map[string]string{".localci.yml": "project: x\n"})
	b, ok, err := ShowFile(ctx, repo, sha, ".localci.yml")
	if err != nil || !ok || string(b) != "project: x\n" {
		t.Fatalf("ShowFile = %q, %v, %v", b, ok, err)
	}
	if _, ok, err := ShowFile(ctx, repo, sha, "missing.yml"); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}
}

func TestClearLocalEnv(t *testing.T) {
	t.Setenv("GIT_DIR", "/nonexistent")
	t.Setenv("GIT_WORK_TREE", "/nonexistent")
	if err := ClearLocalEnv(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		if _, ok := os.LookupEnv(v); ok {
			t.Errorf("%s still set", v)
		}
	}
}

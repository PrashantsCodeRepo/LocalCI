package localci

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PrashantsCodeRepo/LocalCI/internal/runner"
)

// fakeGH puts a gh on PATH that logs its args and answers the commit lookup from a flag file.
func fakeGH(t *testing.T) (calls, onGitHub string) {
	t.Helper()
	dir := t.TempDir()
	calls, onGitHub = filepath.Join(dir, "calls"), filepath.Join(dir, "on-github")
	script := `#!/bin/sh
echo "$*" >> ` + calls + `
case "$*" in *commits/*) [ -e ` + onGitHub + ` ] ;; esac
`
	os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls, onGitHub
}

func outcome(sha string, passed bool) *Outcome {
	r := runner.Result{Group: "g", Step: "s", Outcome: runner.Pass}
	if !passed {
		r.Outcome = runner.Fail
	}
	return &Outcome{SHA: sha, Passed: passed, Results: []runner.Result{r}}
}

func TestPostPendingWaitsForTheCommitThenPostsTheLatestResult(t *testing.T) {
	calls, onGitHub := fakeGH(t)
	root, repo, sha := t.TempDir(), t.TempDir(), strings.Repeat("c", 40)
	if err := SavePending(root, outcome(sha, false), nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- PostPending(context.Background(), root, repo, sha, 5*time.Second, 20*time.Millisecond, &bytes.Buffer{})
	}()
	time.Sleep(100 * time.Millisecond)
	// A rerun of the same commit passes before the push lands.
	if err := SavePending(root, outcome(sha, true), nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(onGitHub, nil, 0o644)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(calls)
	if !strings.Contains(string(got), "statuses/"+sha) || !strings.Contains(string(got), "state=success") || strings.Contains(string(got), "state=failure") {
		t.Errorf("gh calls:\n%s\nwant only the latest (success) statuses posted", got)
	}
	if _, err := os.Stat(pendingPath(root, sha)); !os.IsNotExist(err) {
		t.Errorf("pending file left behind: %v", err)
	}
	// A second poster for the same commit finds nothing to do.
	if err := PostPending(context.Background(), root, repo, sha, time.Second, 10*time.Millisecond, &bytes.Buffer{}); err != nil {
		t.Errorf("second poster: %v", err)
	}
}

func TestPostPendingGivesUpWhenThePushNeverLands(t *testing.T) {
	calls, _ := fakeGH(t)
	root, repo, sha := t.TempDir(), t.TempDir(), strings.Repeat("d", 40)
	SavePending(root, outcome(sha, true), nil)
	err := PostPending(context.Background(), root, repo, sha, 100*time.Millisecond, 20*time.Millisecond, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "did not reach GitHub") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	got, _ := os.ReadFile(calls)
	if strings.Contains(string(got), "statuses/") {
		t.Errorf("posted a status for a commit GitHub does not have:\n%s", got)
	}
}

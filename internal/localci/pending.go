package localci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/PrashantsCodeRepo/LocalCI/internal/report"
)

// A pre-push hook runs before git uploads anything, and GitHub refuses a status for a commit
// it does not have. So the hook saves the statuses and starts a detached `localci post-pending`
// that waits for the commit to reach GitHub and then posts them.

func pendingPath(cacheRoot, sha string) string {
	return filepath.Join(cacheRoot, "runs", sha+".pending.json")
}

// SavePending writes the statuses for o to the cache so PostPending can post them later.
// It writes a temp file and renames it, so a poster never reads a half-written file.
func SavePending(cacheRoot string, o *Outcome, runErr error) error {
	data, err := json.Marshal(Statuses(o, runErr))
	if err != nil {
		return err
	}
	path := pendingPath(cacheRoot, o.SHA)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// StartPostPending starts `<bin> post-pending <sha>` in its own session, with no stdin and
// its output in <sha>.post.log, so it outlives the hook and git does not wait for it.
func StartPostPending(cacheRoot, bin, repo, sha string) (string, error) {
	logPath := filepath.Join(cacheRoot, "runs", sha+".post.log")
	// Append: a push of two refs at one SHA starts two posters, and neither may truncate the other.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cmd := exec.Command(bin, "post-pending", sha)
	cmd.Dir = repo
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	return logPath, cmd.Process.Release()
}

// PostPending waits until GitHub has the commit, posts the saved statuses and removes the
// file. It gives up after timeout (the push may have been rejected), leaving the file.
// The file is read only once the commit is there, so when a later run of the same commit
// has replaced it (a fail, then a pass), every waiting poster posts the latest result.
func PostPending(ctx context.Context, cacheRoot, repo, sha string, timeout, every time.Duration, w io.Writer) error {
	path := pendingPath(cacheRoot, sha)
	deadline := time.Now().Add(timeout)
	for !commitOnGitHub(ctx, repo, sha) {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not reach GitHub within %s; statuses not posted (saved in %s)", sha[:12], timeout, path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // another poster for this commit already posted
	}
	if err != nil {
		return err
	}
	var statuses []report.Status
	if err := json.Unmarshal(data, &statuses); err != nil {
		return err
	}
	for _, s := range statuses {
		if err := report.PostStatus(ctx, repo, sha, s); err != nil {
			return err
		}
		fmt.Fprintf(w, "localci: posted %s=%s on %s\n", s.Context, s.State, sha[:12])
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func commitOnGitHub(ctx context.Context, repo, sha string) bool {
	cmd := exec.CommandContext(ctx, "gh", "api", "--silent", "repos/{owner}/{repo}/commits/"+sha)
	cmd.Dir = repo
	return cmd.Run() == nil
}

// Package report writes run output to the terminal and a log file, and posts GitHub
// commit statuses.
package report

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Log tees output to w and to <dir>/<sha>.log.
type Log struct {
	io.Writer
	Path string
	f    *os.File
}

// OpenLog creates (or truncates) the run log for sha.
func OpenLog(dir, sha string, w io.Writer) (*Log, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sha+".log")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Log{Writer: io.MultiWriter(w, f), Path: path, f: f}, nil
}

// Close closes the log file.
func (l *Log) Close() error { return l.f.Close() }

// Status is one GitHub commit status.
type Status struct {
	State       string // success, failure, error or pending
	Context     string
	Description string
}

// PostStatus posts a commit status with `gh api`. {owner}/{repo} is filled in by gh from
// the git remote of repo.
func PostStatus(ctx context.Context, repo, sha string, s Status) error {
	desc := s.Description
	if len(desc) > 140 {
		desc = desc[:137] + "..."
	}
	cmd := exec.CommandContext(ctx, "gh", "api", "--method", "POST", "--silent",
		"repos/{owner}/{repo}/statuses/"+sha,
		"-f", "state="+s.State, "-f", "context="+s.Context, "-f", "description="+desc)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh api statuses (%s): %v: %s", s.Context, err, strings.TrimSpace(string(out)))
	}
	return nil
}

package localci

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/PrashantsCodeRepo/LocalCI/internal/config"
	"github.com/PrashantsCodeRepo/LocalCI/internal/gitx"
)

// skipReason reports why the checks can be skipped for sha, or "" when they must run.
// They are skipped only when the commit differs from where it left the default branch
// (origin/HEAD) and every changed path matches a skip_when_only pattern. The code is then
// the default branch's own. An empty diff runs the checks, so checking the default branch
// itself never skips. Without origin/HEAD nothing is skipped. A stale origin/HEAD can only
// widen the diff, which runs more, never less.
func skipReason(ctx context.Context, repo, sha string, patterns []string) (string, error) {
	if len(patterns) == 0 {
		return "", nil
	}
	base, err := gitx.Run(ctx, repo, "rev-parse", "-q", "--verify", "refs/remotes/origin/HEAD^{commit}")
	if err != nil || base == "" {
		return "", nil
	}
	// --no-renames lists both sides of a rename, so moving code into docs/ still runs.
	out, err := gitx.Run(ctx, repo, "diff", "--name-only", "--no-renames", "-z", base+"..."+sha)
	if err != nil {
		return "", err
	}
	var changed []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return "", nil
	}
	for _, p := range changed {
		// A commit that changes the config is always checked, so it cannot widen
		// skip_when_only to cover itself.
		if p == config.FileName || !matchAny(patterns, p) {
			return "", nil
		}
	}
	return fmt.Sprintf("only skip_when_only paths changed (%d files vs origin/HEAD)", len(changed)), nil
}

func matchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if MatchGlob(pat, p) {
			return true
		}
	}
	return false
}

// MatchGlob matches a slash-separated path against a pattern where each segment is a
// path.Match pattern and a "**" segment matches any number of directories, including none.
func MatchGlob(pattern, p string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

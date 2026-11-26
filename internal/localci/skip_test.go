package localci

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchGlob(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{"*.md", "README.md", true},
		{"*.md", "docs/a.md", false},
		{"docs/**/*.md", "docs/a.md", true},
		{"docs/**/*.md", "docs/x/y/a.md", true},
		{"docs/**/*.md", "docs/a.pdf", false},
		{"docs/**/*.md", "src/docs/a.md", false},
		{"docs/**", "docs/x/a.pdf", true},
		{"**/*.md", "a/b/c.md", true},
	} {
		if got := MatchGlob(c.pat, c.path); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

const skipping = `project: demo
skip_when_only: ["*.md", "docs/**/*.md"]
status:
  per_group: true
groups:
  - name: checks
    steps:
      - name: ran
        run: "true"
`

// skipRepo makes a repo whose origin/HEAD points at a first commit with code and config.
func skipRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	base := commit(t, dir, map[string]string{".localci.yml": skipping, "app.go": "package app\n", "README.md": "x\n"})
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", base)
	gitIn(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return dir
}

func runIn(t *testing.T, dir string) *Outcome {
	t.Helper()
	o, err := Run(context.Background(), Options{Dir: dir, Out: &bytes.Buffer{}, CacheRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestDocsOnlyCommitSkipsWithSuccessStatuses(t *testing.T) {
	dir := skipRepo(t)
	commit(t, dir, map[string]string{"README.md": "y\n", "docs/notes/a.md": "z\n"})
	o := runIn(t, dir)
	if o.SkipReason == "" || !o.Passed || len(o.Results) != 0 {
		t.Fatalf("want a skip with no steps run, got reason=%q passed=%v results=%d", o.SkipReason, o.Passed, len(o.Results))
	}
	st := Statuses(o, nil)
	if len(st) != 2 || st[0].State != "success" || !strings.HasPrefix(st[0].Description, "skipped") || st[1].Context != "localci/checks" {
		t.Errorf("statuses = %+v", st)
	}
}

func TestSkipNeedsEveryChangedPathToMatch(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"code change":      {"docs/a.md": "z\n", "app.go": "package app // edit\n"},
		"non-markdown doc": {"docs/source.pdf": "%PDF\n"},
		"config change":    {".localci.yml": skipping + "# edit\n"},
		"config widening the skip to cover itself": {".localci.yml": strings.Replace(skipping,
			`["*.md", "docs/**/*.md"]`, `["**"]`, 1), "app.go": "package app // edit\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := skipRepo(t)
			commit(t, dir, files)
			if o := runIn(t, dir); o.SkipReason != "" || len(o.Results) == 0 {
				t.Errorf("skipped (%q), want the checks to run", o.SkipReason)
			}
		})
	}
}

func TestRenamingCodeIntoDocsStillRuns(t *testing.T) {
	dir := skipRepo(t)
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	gitIn(t, dir, "mv", "app.go", "docs/app.md")
	gitIn(t, dir, "commit", "-qm", "mv")
	if o := runIn(t, dir); o.SkipReason != "" {
		t.Errorf("a rename out of app.go skipped: %q", o.SkipReason)
	}
}

func TestTheDefaultBranchItselfIsNeverSkipped(t *testing.T) {
	dir := skipRepo(t) // HEAD == origin/HEAD: empty diff
	if o := runIn(t, dir); o.SkipReason != "" || len(o.Results) == 0 {
		t.Errorf("origin/HEAD itself skipped: %q", o.SkipReason)
	}
}

func TestNoOriginHEADNeverSkips(t *testing.T) {
	dir := skipRepo(t)
	gitIn(t, dir, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	commit(t, dir, map[string]string{"README.md": "y\n"})
	if o := runIn(t, dir); o.SkipReason != "" {
		t.Errorf("skipped without origin/HEAD: %q", o.SkipReason)
	}
}

package runner

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PrashantsCodeRepo/LocalCI/internal/config"
)

func newRunner(t *testing.T) (*Runner, *bytes.Buffer) {
	var buf bytes.Buffer
	return &Runner{Root: t.TempDir(), Env: os.Environ(), Out: &buf, FailFast: true}, &buf
}

func outcomes(rs []Result) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Step+"="+string(r.Outcome))
	}
	return strings.Join(s, " ")
}

func TestFailFastStopsLaterStepsAndGroups(t *testing.T) {
	r, _ := newRunner(t)
	rs := r.Run(context.Background(), []config.Group{
		{Name: "g1", Steps: []config.Step{
			{Name: "ok", Run: "true"},
			{Name: "bad", Run: "exit 3"},
			{Name: "after", Run: "true"},
		}},
		{Name: "g2", Steps: []config.Step{{Name: "later", Run: "true"}}},
	})
	want := "ok=pass bad=fail after=not run later=not run"
	if got := outcomes(rs); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestInformationalNeverFails(t *testing.T) {
	r, buf := newRunner(t)
	rs := r.Run(context.Background(), []config.Group{{Name: "g", Steps: []config.Step{
		{Name: "info", Run: "exit 1", Informational: true},
		{Name: "next", Run: "true"},
	}}})
	if got := outcomes(rs); got != "info=fail (informational) next=pass" {
		t.Errorf("got %s", got)
	}
	if !Summary(buf, rs) {
		t.Error("informational failure failed the run")
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	r, _ := newRunner(t)
	start := time.Now()
	// The child sleep would keep the pipe open if only bash were killed.
	rs := r.Run(context.Background(), []config.Group{{Name: "g", Steps: []config.Step{
		{Name: "slow", Run: "sleep 30 & wait", Timeout: config.Duration(300 * time.Millisecond)},
	}}})
	if rs[0].Outcome != TimedOut {
		t.Errorf("outcome %s, want timed out", rs[0].Outcome)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("took %s; the process group was not killed", el)
	}
}

func TestStdinIsClosed(t *testing.T) {
	r, _ := newRunner(t)
	rs := r.Run(context.Background(), []config.Group{{Name: "g", Steps: []config.Step{
		// read returns non-zero at EOF; with an open stdin it would block until the timeout.
		{Name: "read", Run: "if read -r x; then exit 1; fi", Timeout: config.Duration(5 * time.Second)},
	}}})
	if rs[0].Outcome != Pass {
		t.Errorf("outcome %s: stdin was not at EOF", rs[0].Outcome)
	}
}

func TestCwdEnvAndExpansion(t *testing.T) {
	r, _ := newRunner(t)
	os.Mkdir(r.Root+"/sub", 0o755)
	r.Env = append(r.Env, "BASE=root")
	rs := r.Run(context.Background(), []config.Group{{Name: "g", Steps: []config.Step{{
		Name: "check", Cwd: "sub",
		Env: map[string]string{"X": "${BASE}/x"},
		Run: `[ "$(basename "$PWD")" = sub ] && [ "$X" = root/x ]`,
	}}}})
	if rs[0].Outcome != Pass {
		t.Errorf("outcome %s", rs[0].Outcome)
	}
}

func TestPipefail(t *testing.T) {
	r, _ := newRunner(t)
	rs := r.Run(context.Background(), []config.Group{{Name: "g", Steps: []config.Step{
		{Name: "pipe", Run: "false | cat"},
	}}})
	if rs[0].Outcome != Fail {
		t.Errorf("a failing pipeline passed")
	}
}

func TestParallelGroupRunsConcurrentlyAndKeepsOutputWhole(t *testing.T) {
	r, buf := newRunner(t)
	start := time.Now()
	rs := r.Run(context.Background(), []config.Group{{Name: "g", Parallel: true, Steps: []config.Step{
		{Name: "a", Run: "echo a1; sleep 0.5; echo a2"},
		{Name: "b", Run: "echo b1; sleep 0.5; echo b2"},
	}}})
	if el := time.Since(start); el > 900*time.Millisecond {
		t.Errorf("parallel steps took %s; not concurrent", el)
	}
	if outcomes(rs) != "a=pass b=pass" {
		t.Errorf("got %s", outcomes(rs))
	}
	out := buf.String()
	if !strings.Contains(out, "a1\na2") || !strings.Contains(out, "b1\nb2") {
		t.Errorf("output interleaved:\n%s", out)
	}
}

func TestTierFullSkippedByDefault(t *testing.T) {
	r, _ := newRunner(t)
	g := []config.Group{{Name: "g", Steps: []config.Step{{Name: "heavy", Run: "true", Tier: "full"}}}}
	if rs := r.Run(context.Background(), g); rs[0].Outcome != Skipped {
		t.Errorf("default tier: %s", rs[0].Outcome)
	}
	r.Tier = "full"
	if rs := r.Run(context.Background(), g); rs[0].Outcome != Pass {
		t.Errorf("tier full: %s", rs[0].Outcome)
	}
}

func TestCancelStopsRun(t *testing.T) {
	r, _ := newRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	rs := r.Run(ctx, []config.Group{{Name: "g", Steps: []config.Step{
		{Name: "long", Run: "sleep 30"},
		{Name: "never", Run: "true"},
	}}})
	if got := outcomes(rs); got != "long=cancelled never=not run" {
		t.Errorf("got %s", got)
	}
}

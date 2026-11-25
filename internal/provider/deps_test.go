package provider

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestEnsureInstallsOnceAndReuses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dep-abc")
	calls := 0
	install := func(d string) error {
		calls++
		return os.WriteFile(filepath.Join(d, "lib"), []byte("x"), 0o644)
	}
	installed, release, err := Ensure(dir, install)
	if err != nil || !installed {
		t.Fatalf("first Ensure: installed=%v err=%v", installed, err)
	}
	release()
	installed, release, err = Ensure(dir, install)
	if err != nil || installed {
		t.Fatalf("second Ensure: installed=%v err=%v", installed, err)
	}
	defer release()
	if calls != 1 {
		t.Errorf("install ran %d times", calls)
	}
}

func TestEnsureRemovesDirWhenInstallFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dep-abc")
	_, _, err := Ensure(dir, func(d string) error {
		os.WriteFile(filepath.Join(d, "partial"), nil, 0o644)
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("failed install left %s behind", dir)
	}
}

func TestEnsureRedoesIncompleteInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dep-abc")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "half"), nil, 0o644) // no .complete: a killed install
	installed, release, err := Ensure(dir, func(string) error { return nil })
	if err != nil || !installed {
		t.Fatalf("installed=%v err=%v", installed, err)
	}
	release()
	if _, err := os.Stat(filepath.Join(dir, "half")); !os.IsNotExist(err) {
		t.Error("leftover from the killed install survived")
	}
}

func TestEnsureMarksInUseUntilRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dep-abc")
	_, release, err := Ensure(dir, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !inUse(dir) {
		t.Error("not marked in use")
	}
	release()
	if inUse(dir) {
		t.Error("still in use after release")
	}
}

func TestPruneKeepsNewestAndInUse(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(root, name)
		os.MkdirAll(p, 0o755)
		ts := time.Now().Add(-age)
		os.Chtimes(p, ts, ts)
		return p
	}
	newest := mk("web-10000000000000000000", 1*time.Hour)
	second := mk("web-20000000000000000000", 2*time.Hour)
	oldLive := mk("web-30000000000000000000", 3*time.Hour)
	oldDead := mk("web-40000000000000000000", 4*time.Hour)
	oldNoMarker := mk("web-50000000000000000000", 5*time.Hour)
	lookalike := mk("web-extra-00000000000000000000", 9*time.Hour) // another dep whose name starts with web-
	other := mk("backend-10000000000000000000", 9*time.Hour)

	os.WriteFile(filepath.Join(oldLive, ".inuse."+strconv.Itoa(os.Getpid())), nil, 0o644)
	os.WriteFile(filepath.Join(oldDead, ".inuse."+strconv.Itoa(deadPID(t))), nil, 0o644)
	// Re-set times: writing the markers bumped the directories' mtimes.
	for p, age := range map[string]time.Duration{oldLive: 3 * time.Hour, oldDead: 4 * time.Hour} {
		ts := time.Now().Add(-age)
		os.Chtimes(p, ts, ts)
	}

	if err := Prune(root, "web", 2); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{newest: true, second: true, oldLive: true, oldDead: false, oldNoMarker: false, other: true, lookalike: true} {
		_, err := os.Stat(p)
		if exists := err == nil; exists != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), exists, want)
		}
	}
}

// deadPID returns the PID of a process that has exited.
func deadPID(t *testing.T) int {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

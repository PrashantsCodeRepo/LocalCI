package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(`
project: demo
postgres: {}
deps:
  - {name: web, kind: npm}
groups:
  - name: g
    steps:
      - {name: s, run: "true", timeout: 90s}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.FailFastOn() || c.StatusContext() != "localci" {
		t.Error("fail_fast or status context default wrong")
	}
	if c.Postgres.URL != "postgresql://localhost:5432" || c.Postgres.Env != "DATABASE_URL" || len(c.Postgres.Create) != 1 {
		t.Errorf("postgres defaults: %+v", c.Postgres)
	}
	if c.Deps[0].Dir != "." || c.Deps[0].Keep != 3 {
		t.Errorf("deps defaults: %+v", c.Deps[0])
	}
	if time.Duration(c.Groups[0].Steps[0].Timeout) != 90*time.Second {
		t.Error("timeout not parsed")
	}
}

func TestParseRejects(t *testing.T) {
	for name, yml := range map[string]string{
		"unknown field":  "project: x\nbogus: 1\ngroups: [{name: g, steps: [{name: s, run: x}]}]",
		"bad project":    "project: My-App\ngroups: [{name: g, steps: [{name: s, run: x}]}]",
		"no groups":      "project: x",
		"step no run":    "project: x\ngroups: [{name: g, steps: [{name: s}]}]",
		"bad kind":       "project: x\ndeps: [{name: d, kind: gem}]\ngroups: [{name: g, steps: [{name: s, run: x}]}]",
		"python missing": "project: x\ndeps: [{name: d, kind: python}]\ngroups: [{name: g, steps: [{name: s, run: x}]}]",
		"bad suffix":     "project: x\npostgres: {suffixes: [\"; drop\"]}\ngroups: [{name: g, steps: [{name: s, run: x}]}]",
		"digit suffix":   "project: x\npostgres: {suffixes: [\"2_test\"]}\ngroups: [{name: g, steps: [{name: s, run: x}]}]",
		"bad tier":       "project: x\ngroups: [{name: g, steps: [{name: s, run: x, tier: huge}]}]",
		"bad toolchain":  "project: x\ntoolchains: {ruby: \"3\"}\ngroups: [{name: g, steps: [{name: s, run: x}]}]",
	} {
		if _, err := Parse([]byte(yml)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), FileName) {
			t.Errorf("%s: error does not name the file: %v", name, err)
		}
	}
}

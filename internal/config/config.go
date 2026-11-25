// Package config parses .localci.yml.
package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the config file LocalCI reads from the commit under test.
const FileName = ".localci.yml"

// Config is the whole of .localci.yml.
type Config struct {
	// Project names the project in database names and cache paths.
	Project string `yaml:"project"`
	// Requires lists commands that must be on PATH; the run fails before any step if one is
	// missing. Use it for tools whose absence makes tests skip instead of fail.
	Requires []string `yaml:"requires"`
	// Toolchains pins tool versions, e.g. {node: "22"}. Each resolves to an installed binary
	// whose directory goes first on PATH, or the run fails.
	Toolchains map[string]string `yaml:"toolchains"`
	// Env is set for every step. ${LOCALCI_WORKTREE} and other variables are expanded.
	Env      map[string]string `yaml:"env"`
	Postgres *Postgres         `yaml:"postgres"`
	Deps     []Dep             `yaml:"deps"`
	// FailFast stops at the first failing step. Default true.
	FailFast *bool        `yaml:"fail_fast"`
	Status   StatusConfig `yaml:"status"`
	Groups   []Group      `yaml:"groups"`
}

// StatusConfig controls the GitHub commit statuses.
type StatusConfig struct {
	// Context is the overall status context. Default "localci".
	Context string `yaml:"context"`
	// PerGroup also posts "<context>/<group>" for each group.
	PerGroup bool `yaml:"per_group"`
}

// Postgres is a per-run database on an existing server.
type Postgres struct {
	// URL of the server, without a database name. Default postgresql://localhost:5432.
	// The LOCALCI_PG environment variable overrides it.
	URL string `yaml:"url"`
	// Env is the variable that receives the per-run database URL. Default DATABASE_URL.
	Env string `yaml:"env"`
	// Suffixes are databases derived from the per-run name (e.g. _test, _eval) by the
	// project's own code. All are dropped at start and on exit.
	Suffixes []string `yaml:"suffixes"`
	// Create lists suffixes ("" for the base name) that LocalCI creates and connects to
	// before any step, failing loudly if it cannot. Default [""].
	Create []string `yaml:"create"`
}

// Dep is a cached dependency install.
type Dep struct {
	Name string `yaml:"name"`
	// Kind is npm or python.
	Kind string `yaml:"kind"`
	// Dir (npm) holds package.json and package-lock.json; node_modules is linked there.
	Dir string `yaml:"dir"`
	// Python (python) is the interpreter version for the venv, e.g. "3.14".
	Python string `yaml:"python"`
	// Manifests (python) are hashed into the cache key, e.g. [backend/pyproject.toml].
	Manifests []string `yaml:"manifests"`
	// Install (python) are the pip install arguments, run from the worktree root.
	Install []string `yaml:"install"`
	// Keep is how many installs of this dep to keep. Default 3.
	Keep int `yaml:"keep"`
}

// Group is a named set of steps.
type Group struct {
	Name     string `yaml:"name"`
	Parallel bool   `yaml:"parallel"`
	Steps    []Step `yaml:"steps"`
}

// Step is one shell command.
type Step struct {
	Name string `yaml:"name"`
	// Run is a bash script, run with -eo pipefail.
	Run           string            `yaml:"run"`
	Cwd           string            `yaml:"cwd"`
	Env           map[string]string `yaml:"env"`
	Timeout       Duration          `yaml:"timeout"`
	Informational bool              `yaml:"informational"`
	// Tier "full" steps run only with --tier full.
	Tier string `yaml:"tier"`
}

// Duration is a time.Duration written as "10m" in YAML.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// DefaultTimeout applies to steps with no timeout.
const DefaultTimeout = 30 * time.Minute

var projectRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Parse reads and validates a config.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &c, nil
}

// FailFastOn reports whether the run stops at the first failure.
func (c *Config) FailFastOn() bool { return c.FailFast == nil || *c.FailFast }

// StatusContext is the overall GitHub status context.
func (c *Config) StatusContext() string {
	if c.Status.Context == "" {
		return "localci"
	}
	return c.Status.Context
}

func (c *Config) validate() error {
	if !projectRE.MatchString(c.Project) {
		return fmt.Errorf("project %q must match %s (it is used in database names)", c.Project, projectRE)
	}
	for name := range c.Toolchains {
		if name != "node" {
			return fmt.Errorf("toolchain %q: only node is supported (python versions are set per dep)", name)
		}
	}
	if p := c.Postgres; p != nil {
		if p.URL == "" {
			p.URL = "postgresql://localhost:5432"
		}
		if p.Env == "" {
			p.Env = "DATABASE_URL"
		}
		if p.Create == nil {
			p.Create = []string{""}
		}
		for _, s := range append(append([]string{}, p.Suffixes...), p.Create...) {
			if s != "" && !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(s) {
				return fmt.Errorf("postgres suffix %q: use lowercase letters, digits and _", s)
			}
			// <project>_localci_<8 hex>_<pid up to 7 digits><suffix> must fit Postgres's 63 bytes.
			if n := len(c.Project) + len("_localci_") + 8 + 1 + 7 + len(s); n > 63 {
				return fmt.Errorf("postgres: project %q with suffix %q makes database names too long (%d > 63 bytes)", c.Project, s, n)
			}
		}
	}
	seen := map[string]bool{}
	for i := range c.Deps {
		d := &c.Deps[i]
		if d.Name == "" || seen[d.Name] {
			return fmt.Errorf("deps[%d]: name missing or repeated", i)
		}
		seen[d.Name] = true
		if d.Keep == 0 {
			d.Keep = 3
		}
		switch d.Kind {
		case "npm":
			if d.Dir == "" {
				d.Dir = "."
			}
		case "python":
			if d.Python == "" || len(d.Manifests) == 0 || len(d.Install) == 0 {
				return fmt.Errorf("deps %s: python needs python, manifests and install", d.Name)
			}
		default:
			return fmt.Errorf("deps %s: kind must be npm or python", d.Name)
		}
	}
	if len(c.Groups) == 0 {
		return fmt.Errorf("no groups")
	}
	for _, g := range c.Groups {
		if g.Name == "" {
			return fmt.Errorf("a group has no name")
		}
		for _, s := range g.Steps {
			if s.Name == "" || strings.TrimSpace(s.Run) == "" {
				return fmt.Errorf("group %s: every step needs a name and run", g.Name)
			}
			if s.Tier != "" && s.Tier != "full" {
				return fmt.Errorf("step %s: tier must be empty or full", s.Name)
			}
		}
	}
	return nil
}

// Package provider sets up what steps need: toolchains, dependency caches and databases.
package provider

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// toolchainCandidates lists where an installed version is looked for, most specific first.
// Homebrew's versioned formulas live under opt/<name>@<version>.
func toolchainCandidates(tool, version string) []string {
	switch tool {
	case "node":
		var c []string
		for _, prefix := range []string{"/opt/homebrew", "/usr/local"} {
			c = append(c, filepath.Join(prefix, "opt", "node@"+version, "bin", "node"))
		}
		if p, err := exec.LookPath("node" + version); err == nil {
			c = append(c, p)
		}
		if p, err := exec.LookPath("node"); err == nil {
			c = append(c, p)
		}
		return c
	case "python":
		var c []string
		if p, err := exec.LookPath("python" + version); err == nil {
			c = append(c, p)
		}
		for _, prefix := range []string{"/opt/homebrew", "/usr/local"} {
			c = append(c, filepath.Join(prefix, "opt", "python@"+version, "bin", "python"+version))
		}
		return c
	}
	return nil
}

// versionMatches reports whether `<bin> --version` output is the pinned version, e.g.
// "v22.11.0" for node 22 or "Python 3.14.0" for python 3.14.
func versionMatches(tool, version, out string) bool {
	out = strings.TrimSpace(out)
	switch tool {
	case "node":
		return strings.HasPrefix(out, "v"+version+".") || out == "v"+version
	case "python":
		v := strings.TrimPrefix(out, "Python ")
		return strings.HasPrefix(v, version+".") || v == version
	}
	return false
}

// ResolveToolchain finds an installed binary for tool at version, failing loudly if none.
func ResolveToolchain(tool, version string) (string, error) {
	cands := toolchainCandidates(tool, version)
	if cands == nil {
		return "", fmt.Errorf("unknown toolchain %q", tool)
	}
	var tried []string
	for _, c := range cands {
		out, err := exec.Command(c, "--version").CombinedOutput()
		if err != nil {
			tried = append(tried, c+" (missing)")
			continue
		}
		if versionMatches(tool, version, string(out)) {
			return c, nil
		}
		tried = append(tried, fmt.Sprintf("%s (%s)", c, strings.TrimSpace(string(out))))
	}
	return "", fmt.Errorf("%s %s not installed; tried: %s", tool, version, strings.Join(tried, ", "))
}

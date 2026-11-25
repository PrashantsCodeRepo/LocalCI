package localci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PrashantsCodeRepo/LocalCI/internal/gitx"
)

const hookMarker = "# localci pre-push hook"

// HookScript is the pre-push hook. It checks every commit being pushed (not deletions) and
// blocks the push if one fails. `localci push` sets LOCALCI_SKIP because it has already run.
func HookScript(bin string) string {
	return `#!/bin/sh
` + hookMarker + ` (written by "localci hook install")
[ -n "$LOCALCI_SKIP" ] && exit 0
LOCALCI=$(command -v localci || echo '` + strings.ReplaceAll(bin, "'", `'\''`) + `')
z=0000000000000000000000000000000000000000
while read -r lref lsha rref rsha; do
  [ "$lsha" = "$z" ] && continue
  "$LOCALCI" run "$lsha" </dev/null || exit 1
done
`
}

// InstallHook writes the pre-push hook into the hooks directory git uses (core.hooksPath
// included). It refuses to replace a hook it did not write unless force is set.
func InstallHook(ctx context.Context, dir string, force bool) (string, error) {
	repo, err := gitx.Toplevel(ctx, dir)
	if err != nil {
		return "", err
	}
	rel, err := gitx.Run(ctx, repo, "rev-parse", "--git-path", "hooks/pre-push")
	if err != nil {
		return "", err
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, rel)
	}
	if old, err := os.ReadFile(path); err == nil && !strings.Contains(string(old), hookMarker) && !force {
		return "", fmt.Errorf("%s exists and was not written by localci; call `localci run` from it, or pass --force to replace it", path)
	}
	bin, err := os.Executable()
	if err != nil {
		bin = "localci"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(HookScript(bin)), 0o755)
}

# LocalCI

LocalCI runs a project's CI checks on your own machine, against the exact commit you are
about to push, and can post the result to GitHub as a commit status. It is one Go binary:
no server, no web UI, no container engine.

```
localci run [<rev>]      check a commit (default HEAD) in a throwaway worktree
localci push [-- args]   check HEAD, then git push, then post the GitHub status
localci hook install     write a pre-push hook that runs the checks
```

## Why

Hosted CI minutes cost money, and most of what a small project's CI does can run on the
laptop that wrote the code. The hard part is making a local run trustworthy:

- It must check the commit being pushed, not whatever is in the working tree.
- Each commit must be judged by its own checks, so a branch that adds a check is held to it.
- A test that silently skips (no database, missing tool) must fail the run instead.
- Concurrent runs must not share databases or dependency installs that are being changed.

Existing tools cover parts of this. Lefthook and pre-commit manage hooks but run against
the working tree, not the pushed commit in isolation. Woodpecker's local exec runs steps
with no isolation. Dagger needs Docker and pipelines rewritten as SDK code. gh-signoff
posts a commit status but runs nothing. LocalCI is the piece in between.

## Install

```bash
go install github.com/PrashantsCodeRepo/LocalCI/cmd/localci@latest
```

Or clone and `go build -o ~/bin/localci ./cmd/localci`. It needs `git` and `bash`, plus
`gh` (logged in) for commit statuses.

## How a run works

1. Clear every variable `git rev-parse --local-env-vars` lists. Git exports `GIT_DIR` and
   others to hooks; left set, they point git commands in tests that build their own temp
   repositories back at your repository.
2. Resolve the revision to a SHA and read `.localci.yml` from that commit
   (`git show <sha>:.localci.yml`), never from the working tree.
3. Add a detached worktree at the SHA in a temp directory. Uncommitted and untracked files
   never reach it.
4. Set up providers: toolchain pins, dependency caches, a per-run Postgres database.
5. Run the step groups. Each step gets `/dev/null` as stdin, its own process group (a
   timeout or Ctrl-C kills the whole tree), and a timeout (default 30m).
6. Print a summary, write the log to `~/.cache/localci/runs/<sha>.log`, and with `--status`
   or `localci push` post the GitHub status.
7. Tear everything down: drop the databases, release the dependency caches, remove the
   worktree. This happens on every exit path, including SIGINT, SIGTERM and SIGHUP.

Steps get `CI=true`, `LOCALCI=1`, `LOCALCI_SHA` and `LOCALCI_WORKTREE` (the worktree root).

## `.localci.yml`

```yaml
project: myapp              # lowercase; used in database names and cache paths

requires: [ffmpeg]          # commands that must be on PATH, or the run fails up front

toolchains:
  node: "22"                # resolves to an installed node 22, put first on PATH

env:                        # for every step; ${VARS} are expanded
  PYTHONPATH: ${LOCALCI_WORKTREE}/backend

postgres:
  url: postgresql://localhost:5432   # the server; LOCALCI_PG overrides it
  env: DATABASE_URL                  # receives postgresql://.../myapp_localci_<pid>
  suffixes: [_test, _eval]           # databases your code derives from that name (no leading digit)
  create: [_test]                    # created and connected to before any step

deps:
  - name: backend
    kind: python
    python: "3.14"
    manifests: [backend/pyproject.toml]
    install: ["./backend[dev]"]      # pip install arguments, run from the worktree root
  - name: web
    kind: npm
    dir: web                         # holds package.json and package-lock.json

fail_fast: true             # default: stop at the first failing step

status:
  context: localci          # default
  per_group: true           # also post localci/<group>

groups:
  - name: backend
    steps:
      - name: lint
        cwd: backend
        run: ruff check .
      - name: types
        cwd: backend
        run: mypy app
        informational: true          # reported, never fails the run
      - name: tests
        cwd: backend
        run: pytest -q
        timeout: 20m
  - name: web
    parallel: true                   # steps in this group run at once; output is kept whole
    steps:
      - {name: typecheck, cwd: web, run: npx tsc --noEmit}
      - {name: unit, cwd: web, run: npm test}
      - {name: e2e, cwd: web, run: npm run e2e, tier: full}   # only with --tier full
```

`run` is a bash script run with `-eo pipefail`. Groups run in order.

### Postgres

LocalCI uses an existing server (a native local install is the default case). Each run
gets its own database name, `<project>_localci_<hostid>_<pid>`, where `<hostid>` is a short
hash of the hostname. Before any step it drops that name plus every declared suffix, drops
databases that this host's earlier runs left behind if their PID is no longer alive (it never
touches another host's databases on a shared server), then creates each `create` database and connects to it. If it cannot, the run fails.
This matters because many test fixtures `pytest.skip` when they cannot create their
database, which would otherwise pass with no database tests run. On exit every one of
those databases is dropped.

### Dependency caches

Installs live under `~/.cache/localci/deps/<project>/<name>-<key>`:

- `npm`: the key covers `package.json`, `package-lock.json` and the node version. Both
  files are in the key because `npm ci` refuses manifests that disagree; a key on the
  lockfile alone would reuse an old install and pass. If `package.json` has its own install
  scripts (`preinstall`, `install`, `postinstall`, `prepare`), those may read any file in
  the repository. If it declares `workspaces`, those packages are linked from their own
  directories. In either case LocalCI runs `npm ci` in the worktree instead and does not cache
  it.
- `python`: a venv made with the pinned interpreter. The key covers the manifests, the
  install arguments and the exact Python version, not the project's own source. So each run
  also installs this commit's copy of every local path in `install` (such as `./backend`)
  into a per-run overlay with `--no-deps`. The overlay comes first on `PYTHONPATH` and
  `PATH`, and the shared venv is never modified. If you set `PYTHONPATH` in `env`, include
  `${PYTHONPATH}` to keep the overlay. Building a local path can write files into the
  worktree, so the worktree is cleaned (`git clean -ffdx`) before the steps run.

An install happens once per key, under a file lock, using the same toolchain the steps
use. A failed or interrupted install is removed. Each run marks the install it uses with
`.inuse.<pid>`; pruning keeps the `keep` (default 3) most recent installs per dep and never
removes one whose marker PID is alive.

### Toolchain pins

`toolchains: {node: "22"}` looks for Homebrew's `node@22`, then `node22`, then `node`, and
checks `--version`. If none matches, the run fails and lists what it tried. A per-run `node`
symlink to the binary it found goes first on `PATH`, so `node`, and npm's
`#!/usr/bin/env node`, run that version even when it is installed as `node22`. Python versions
are pinned per dependency the same way (`python3.14`, then `python@3.14`).

## GitHub status

`localci push` runs the checks on HEAD. If they pass it runs `git push` with your
arguments, then posts the
`localci` status (and `localci/<group>` with `per_group`) through `gh api`. A pre-push hook
cannot post right away, because GitHub does not have the commit until the push finishes
(see Hooks for how it posts later). During that push, `LOCALCI_CHECKED` tells the pre-push hook which SHA was just checked. The
hook skips only refs at that SHA, so any other branch the push arguments include is still
checked.
`localci run --status <rev>` posts for a commit that is already on GitHub.

**The status is self-reported.** Anyone with push access can post any status with `gh
api`. Treat it as a convenience gate against forgetting to run the checks, not as a
security boundary. It says nothing a reviewer should rely on to prove the code was tested.

## Hooks

`localci hook install` writes a pre-push hook into whatever directory git uses for hooks
(`core.hooksPath` included). It runs `localci run --status-after-push <sha>` for each commit
being pushed and blocks the push if one fails. It will not replace a hook it did not write
unless you pass `--force`. Reinstall it after upgrading localci, since the hook text changes.

The hook runs the binary that installed it, and falls back to `localci` on PATH only if that
file is gone.

`--status-after-push` posts the statuses once the push lands, so a plain `git push` gates
the PR the same way `localci push` does. Only a passing run schedules a post; a failing one
blocks the push, so there is nothing to post. The run saves its statuses to
`~/.cache/localci/runs/<sha>.pending.json` and starts a detached `localci post-pending <sha>`.
That process checks every 5 seconds whether GitHub has the commit, posts the saved statuses,
and removes the file. It reads the file only once the commit is there, so if a later run of
the same commit replaced it, the latest result is the one posted. If the commit does not
arrive within 10 minutes (the push was rejected, or the remote is not GitHub), it gives up
and leaves the file. Its output is in `<sha>.post.log` next to the run log.

With Lefthook or pre-commit, call `localci run` from your existing config instead:

```yaml
# lefthook.yml
pre-push:
  commands:
    localci:
      run: localci run
```

## Exit codes

`0` all checks passed, `1` a check failed, `2` the checks could not run (bad config,
missing tool, unreachable database, interrupted).

## Not built yet

Service processes (start a server, wait on a health URL, inject its URL), shared read-only
assets (model weights by checksum), path filters, and a Docker-backed Postgres are planned
but not implemented.

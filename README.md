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
  suffixes: [_test, _eval]           # databases your code derives from that name
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
gets its own database name, `<project>_localci_<pid>`. Before any step it drops that name
plus every declared suffix, drops databases left by runs whose PID is no longer alive,
then creates each `create` database and connects to it. If it cannot, the run fails.
This matters because many test fixtures `pytest.skip` when they cannot create their
database, which would otherwise pass with no database tests run. On exit every one of
those databases is dropped.

### Dependency caches

Installs live under `~/.cache/localci/deps/<project>/<name>-<key>`:

- `npm`: the key covers `package.json`, `package-lock.json` and the node version. Both
  files are in the key because `npm ci` refuses manifests that disagree; a key on the
  lockfile alone would reuse an old install and pass.
- `python`: a venv made with the pinned interpreter. The key covers the manifests, the
  install arguments and the exact Python version. A fresh install of a local path can
  write build files into the worktree, so the worktree is cleaned (`git clean -ffdx`)
  before the steps run.

An install happens once per key, under a file lock, using the same toolchain the steps
use. A failed or interrupted install is removed. Each run marks the install it uses with
`.inuse.<pid>`; pruning keeps the `keep` (default 3) most recent installs per dep and never
removes one whose marker PID is alive.

### Toolchain pins

`toolchains: {node: "22"}` looks for Homebrew's `node@22`, then `node22`, then `node`, and
checks `--version`. If none matches, the run fails and lists what it tried. Python versions
are pinned per dependency the same way (`python3.14`, then `python@3.14`).

## GitHub status

`localci push` runs the checks on HEAD. If they pass it runs `git push` with your
arguments (setting `LOCALCI_SKIP=1` so the hook does not run them again), then posts the
`localci` status (and `localci/<group>` with `per_group`) through `gh api`. A pre-push hook
cannot do this itself, because GitHub does not have the commit until the push finishes.
`localci run --status <rev>` posts for a commit that is already on GitHub.

**The status is self-reported.** Anyone with push access can post any status with `gh
api`. Treat it as a convenience gate against forgetting to run the checks, not as a
security boundary. It says nothing a reviewer should rely on to prove the code was tested.

## Hooks

`localci hook install` writes a pre-push hook into whatever directory git uses for hooks
(`core.hooksPath` included). It runs `localci run <sha>` for each commit being pushed and
blocks the push if one fails. It will not replace a hook it did not write unless you pass
`--force`.

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

## Project Context

`drive-git` uses a private Google Drive folder as a git remote. Repositories stay
ordinary local git repos — real branches, diffs and history — while Drive holds
the remote as a chain of `git bundle`s. A single binary provides both the CLI and
git's remote helper, so `git clone gdrive://name` and plain `git push` work.

## Design invariants

These are load-bearing. Changing them needs a deliberate decision, not a drive-by
refactor.

**The remote is an append-only chain of immutable links.** Drive has no atomic
multi-file write and no compare-and-swap (v3 dropped ETag preconditions), so
correctness cannot rest on a lock. Links are named
`NNNN-<parentTip>-<newTip>.bundle[.age]`, nothing is ever overwritten, and a
simultaneous push produces a detectable sibling rather than a lost update. An
interrupted push leaves the previous valid state.

**The `.lock` file is advisory only.** It exists to stop two machines wasting an
upload. Never present it as the safety mechanism.

**Drive permits duplicate filenames in a folder, and race detection depends on
it.** Asserted directly by a live test rather than assumed.

**Bundles name their refs in the `refs/drive/` mirror namespace**, not
`refs/heads`/`refs/tags`. Git can only put a ref's real name into a bundle, and a
remote ref need not exist locally — `git push main:trunk` has no local
`refs/heads/trunk`. The mirror namespace is ours to shape, so it can carry any
remote ref name.

**Links are applied with `git bundle unbundle`**, which verifies prerequisites,
writes objects, and updates no refs. Not `git fetch`: this code path runs inside
the remote helper, under a git process that is already running.

**Last-synced remote state lives in real git refs** under `refs/drive/*`, not a
sidecar state file.

## Layout

- `cmd/drive-git` — entry point; dispatches to the helper when argv[0] is
  `git-remote-gdrive`
- `internal/store` — the chain, locking, push/pull/clone/compact. Backend-agnostic
- `internal/drive` — Drive API backend, with retry/backoff
- `internal/local` — directory backend (`DRIVE_GIT_LOCAL_ROOT`), for tests and as
  an escape hatch
- `internal/helper` — git remote helper protocol
- `internal/gitx` — git CLI wrapper; we shell out rather than use go-git
- `internal/session` — wiring shared by CLI and helper
- `internal/{auth,crypto,config,update,cli}`

## Conventions

- **Shell out to `git`.** Bundles, rev-list and ref plumbing are first-class in
  the CLI; do not reimplement git internals.
- **The helper must never write to stdout** except protocol output. Notices,
  warnings and errors go to stderr.
- **Encryption is opt-in** (`init --encrypt`), fixed at init, recorded in
  `meta.json`. The security boundary is the Google account and the `drive.file`
  scope; age encryption is defence in depth.
- **Destructive operations get a recoverable path where one exists** — `rm` uses
  Drive's trash via the optional `store.Trasher` interface.

## Testing

```sh
make check       # gofmt, vet, build, test — what CI runs
make test-live   # Drive API tests, needs a logged-in account
```

Three layers: the store against an in-memory fake; the helper with real `git`
driving a real built binary over the local-directory backend; and live tests for
the Drive-specific behaviour a directory cannot model. Prefer adding to the layer
that can actually catch the bug.

## Google OAuth constraint

The consent screen stays in "Testing" — publishing an External app needs a
verified domain. Consequence: refresh tokens expire every 7 days, so
`drive-git login` is re-run periodically. `auth.IsLoginExpired` detects this and
prints a plain explanation instead of a raw OAuth error.

## Out of scope

- Reimplementing git internals (objects, packfiles, merge logic)
- Multi-user collaboration, code review, permissions
- A web UI or hosted service

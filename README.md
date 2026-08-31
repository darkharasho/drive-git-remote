# drive-git-remote

A small Go CLI that uses a private Google Drive folder as a git remote, so personal work scripts and productivity apps can be version-controlled without pushing repos to the org's GitHub. Working folders stay ordinary local git repos, so branches, diffs, and history come free.

## Install

```sh
go build -o drive-git ./cmd/drive-git
```

## First run

```sh
drive-git setup           # walks you through creating your own OAuth desktop client
drive-git login           # loopback browser sign-in, token cached at 0600
drive-git install-helper  # so plain git commands work against gdrive:// URLs
```

`setup` guides you through a personal Google Cloud project so no credentials are shared and nothing touches org infrastructure. The CLI requests only the `drive.file` scope, which grants access to files it creates and nothing else in your Drive.

Two things worth knowing about the Google side:

- **Leave the app in "Testing"** and add yourself as a test user. Publishing an External app to production requires a homepage and privacy policy on a domain you've verified in Search Console — not worth it for a personal tool.
- The cost of staying in Testing is that Google expires refresh tokens after **7 days**, so `drive-git login` needs re-running about weekly. The CLI detects this and says so explicitly rather than surfacing a raw OAuth error.
- The "unverified app" warning during sign-in is expected — it's your own client.

## Everyday use

Once the helper is installed, Drive is just a git remote — use ordinary git:

```sh
git clone gdrive://scripts
git push
git pull
git push gdrive main:trunk      # renaming refspecs work
git push gdrive :refs/heads/old # so do deletions
git ls-remote gdrive
```

`drive-git init` and `drive-git clone` add a `gdrive` remote for you.

The wrapper commands remain for the things git has no verb for:

```sh
cd ~/scripts
git init && git add . && git commit -m "initial"

drive-git init            # creates drive-git-remote/scripts in Drive and pushes
drive-git status          # compare local, last-synced, and remote state
drive-git list            # repos stored in Drive
drive-git unlock          # break a stale push lock
drive-git gc              # compact the remote chain
```

`drive-git push` / `pull` / `clone` also still work if you'd rather not install the helper.

Everything lives under a single `drive-git-remote/` folder in your Drive, one subfolder per repo.

## The remote helper

`drive-git install-helper` symlinks the binary as `git-remote-gdrive` on your PATH (default `~/.local/bin`). Git resolves `gdrive://` URLs by looking for that name, and from then on `clone`, `fetch`, `push`, `pull`, and `ls-remote` all work normally. It's the same single binary — invoked under that name it speaks the helper protocol instead of the CLI.

The helper advertises the `fetch` and `push` capabilities, so git keeps ownership of refspecs and remote-tracking refs while delegating object transfer. Objects land via `git bundle unbundle`, which verifies prerequisites, writes objects, and updates no refs.

One limitation: a repo can have one `gdrive` remote. Both the helper and the CLI track remote state in the same `refs/drive/*` namespace — which is what lets you mix `git push` and `drive-git push` in one repo freely — but it means two different Drive repos as remotes of one working copy would collide.

## How the remote works

Drive has no atomic multi-file write and no compare-and-swap, so the remote is an **append-only chain of immutable links**:

```
drive-git-remote/scripts/
  meta.json
  0001-root-3f2a91c4d8be.bundle.age
  0002-3f2a91c4d8be-77c0aa12e3f5.bundle.age
  0003-77c0aa12e3f5-91bb04de77a1.refs.age
```

Each filename encodes its sequence number, the ref-set fingerprint it was built against, and the fingerprint it produces. Every link is a real `git bundle` naming the complete current ref set while carrying only new objects. Nothing is ever overwritten, so:

- An interrupted push leaves the remote in its previous valid state.
- Two machines pushing at once produce two siblings at the same position — detectable, and recoverable by pulling and re-pushing. The loser's upload is withdrawn automatically.
- A push is never destructive, so no failure mode loses history.

A `.lock` file guards pushes, but it's **advisory only** — it exists to stop two machines wasting an upload, not to provide correctness. Correctness comes from the append-only chain. Stale locks expire after 10 minutes; `drive-git unlock` breaks one by hand.

Last-synced remote state is stored as real git refs under `refs/drive/*`, so you can inspect it with plain git and there's no sidecar state file to corrupt. Bundles name their refs in that same `refs/drive/` namespace rather than as `refs/heads`/`refs/tags` — git can only put a ref's real name into a bundle, and a remote ref need not exist locally at all (`git push main:trunk` has no local `refs/heads/trunk`). An unencrypted link is still a plain bundle you can recover by hand:

```sh
git fetch ./0001-root-3f2a91c4d8be.bundle 'refs/drive/heads/*:refs/heads/*'
```

`drive-git gc` compacts a long chain into a single full bundle and archives the old links.

## Pull semantics

`pull` fetches into `refs/drive/*` and then fast-forwards **only the current branch**, and only when the working tree is clean. It never auto-merges and never touches other branches. If you've diverged, it says so and leaves you to resolve it with ordinary `git rebase`/`git merge` against `refs/drive/heads/<branch>`. Use `--fetch-only` to skip the merge entirely.

## Encryption

Bundles are encrypted client-side with [age](https://age-encryption.org) by default. The key lives at `~/.config/drive-git-remote/key.age`.

- **Back that key up.** Without it, the bundles cannot be decrypted.
- Copy it to any other machine you clone from.
- Filenames and sizes are still visible to Drive; this is defence in depth, not full metadata privacy.
- `drive-git init --no-encrypt` opts out per repo.

## Moving to a normal git remote

Nothing here is a custom format — a repo synced this way is an ordinary git repo. To move to GitHub, just `git remote add origin ...` and push.

## Using a local directory instead of Drive

Setting `DRIVE_GIT_LOCAL_ROOT=/some/dir` swaps the Drive API for a plain directory — no OAuth involved. This is how the end-to-end tests run, and it's a usable escape hatch if you'd rather point at a folder something else syncs (a Drive desktop client, a USB stick).

Caveat: a filesystem can't hold two files with the same name, so uploads overwrite where Drive would create a sibling. Simultaneous pushes are therefore *not* detectable in this mode the way they are against Drive.

## Development

```sh
go test ./...
```

Two layers of tests:

- **Store** — against an in-memory fake of the Drive backend: clone/push/pull round trips, incremental bundles, encryption, compaction, lock expiry, chain validation, and the simultaneous-push race (injecting a competing link mid-upload).
- **Helper** — real `git` driving a real built binary installed as `git-remote-gdrive`, backed by a local directory: `git clone gdrive://`, push, pull, non-fast-forward rejection, renaming and deleting refspecs, and an encrypted repo.

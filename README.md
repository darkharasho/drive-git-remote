# drive-git-remote

[![CI](https://github.com/darkharasho/drive-git-remote/actions/workflows/ci.yml/badge.svg)](https://github.com/darkharasho/drive-git-remote/actions/workflows/ci.yml)

Use a private Google Drive folder as a git remote.

Your repositories stay ordinary local git repos — real branches, real diffs, real history — while Drive holds the remote. Nothing is reinvented: pushes are `git bundle`s, and the whole thing is a single static binary that also serves as a git remote helper, so `git clone gdrive://notes` and plain `git push` just work.

Useful when you want somewhere private to keep repos you don't want on a hosting service, synced across your own machines, using storage you already have.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/darkharasho/drive-git-remote/main/install.sh | sh
```

That detects your platform, verifies the download's checksum, installs to `~/.local/bin`, and sets up the `git-remote-gdrive` helper. To read it first — a fair instinct for anything piped to a shell:

```sh
curl -fsSL https://raw.githubusercontent.com/darkharasho/drive-git-remote/main/install.sh -o install.sh
less install.sh && sh install.sh
```

`PREFIX=/usr/local/bin` to install elsewhere, `VERSION=v0.1.0` to pin a release.

<details>
<summary>Other ways</summary>

**Manual** — grab an archive from [releases](https://github.com/darkharasho/drive-git-remote/releases):

```sh
tar -xzf drive-git_v0.1.0_darwin_arm64.tar.gz
mv drive-git ~/.local/bin/ && drive-git install-helper
```

**With Go:**

```sh
go install github.com/darkharasho/drive-git-remote/cmd/drive-git@latest
drive-git install-helper
```

**From source:**

```sh
git clone https://github.com/darkharasho/drive-git-remote && cd drive-git-remote
make install    # builds, installs to ~/.local/bin, sets up the helper
```

Windows: download the `.zip` from releases. The CLI works; `install-helper` uses symlinks, so put `git-remote-gdrive.exe` on your PATH by copying the binary under that name.

</details>

Upgrading later is just `drive-git update`.

## First run

```sh
drive-git setup    # walks you through creating your own Google OAuth client
drive-git login    # browser sign-in, token cached at 0600
```

`setup` walks you through a Google Cloud project of your own, so the credentials are yours and shared with nobody. The CLI requests only the `drive.file` scope, which grants access to files it creates and nothing else in your Drive.

Three things worth knowing about the Google side:

- **Leave the app in "Testing"** and add your account as a test user. Publishing an External app to production requires a homepage and privacy policy on a domain you've verified in Search Console — a lot of ceremony for a personal tool.
- The cost of staying in Testing is that Google expires refresh tokens after **7 days**, so `drive-git login` needs re-running about weekly. The CLI detects this and says so plainly rather than surfacing a raw OAuth error.
- The "unverified app" warning during sign-in is expected — it's your own client.

## Everyday use

Once the helper is installed, Drive is just a git remote — use ordinary git:

```sh
git clone gdrive://notes
git push
git pull
git push gdrive main:trunk      # renaming refspecs work
git push gdrive :refs/heads/old # so do deletions
git ls-remote gdrive
```

`drive-git init` and `drive-git clone` add a `gdrive` remote for you.

The wrapper commands remain for the things git has no verb for:

```sh
cd ~/notes
git init && git add . && git commit -m "initial"

drive-git init            # creates drive-git-remote/notes in Drive and pushes
drive-git init --encrypt  # ...with client-side encryption
drive-git status          # compare local, last-synced, and remote state
drive-git list            # repos stored in Drive
drive-git rm notes        # remove a repo (goes to Drive's trash)
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
drive-git-remote/notes/
  meta.json
  0001-root-3f2a91c4d8be.bundle
  0002-3f2a91c4d8be-77c0aa12e3f5.bundle
  0003-77c0aa12e3f5-91bb04de77a1.refs
```

(With `--encrypt`, each link gains an `.age` suffix.)

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

## Encryption (optional)

Bundles are stored unencrypted by default. The security boundary is your Google account: the CLI holds only the `drive.file` scope, so it can see the files it created and nothing else, and those files live in your personal Drive.

`drive-git init --encrypt` adds client-side [age](https://age-encryption.org) encryption on top of that, which protects against Drive itself and against an account compromise. If you turn it on:

- **Back up `~/.config/drive-git-remote/key.age`.** Without it the bundles cannot be decrypted, by you or anyone.
- Copy it to any other machine you clone from.
- Filenames and sizes stay visible to Drive; this is defence in depth, not metadata privacy.

Encryption is fixed at `init` and recorded in `meta.json`. There's no converting a repo afterwards — re-create it and push again.

## Moving to a normal git remote

Nothing here is a custom format — a repo synced this way is an ordinary git repo with ordinary objects. To move it to any git host, add a remote and push. There's no migration and no export step, which is the point of building on real git rather than a bespoke versioning scheme.

## Using a local directory instead of Drive

Setting `DRIVE_GIT_LOCAL_ROOT=/some/dir` swaps the Drive API for a plain directory — no OAuth involved. This is how the end-to-end tests run, and it's a usable escape hatch if you'd rather point at a folder something else syncs (a Drive desktop client, a USB stick).

Caveat: a filesystem can't hold two files with the same name, so uploads overwrite where Drive would create a sibling. Simultaneous pushes are therefore *not* detectable in this mode the way they are against Drive.

## Development

```sh
make check              # gofmt, vet, build, test — exactly what CI runs
make test-live          # the Drive-API tests, against your logged-in account
make release-snapshot   # cross-compile every release target without publishing
```

Two layers of tests:

- **Store** — against an in-memory fake of the Drive backend: clone/push/pull round trips, incremental bundles, encryption, compaction, lock expiry, chain validation, and the simultaneous-push race (injecting a competing link mid-upload).
- **Helper** — real `git` driving a real built binary installed as `git-remote-gdrive`, backed by a local directory: `git clone gdrive://`, push, pull, non-fast-forward rejection, renaming and deleting refspecs, and an encrypted repo.
- **Live** — the Drive-specific assumptions that a local directory cannot model, chiefly that Drive permits two files with the same name in one folder. Race detection depends on that, so it is asserted directly rather than assumed. Skipped unless `DRIVE_GIT_LIVE=1`.

CI runs the first two on Linux and macOS, and cross-compiles every release target on each push so a broken platform surfaces before tag time.

## Staying current

```sh
drive-git update          # download, verify and install the latest release
drive-git update --check  # just report what is available
```

`update` replaces the binary in place, after verifying its SHA-256 against the release's `checksums.txt` — a self-updater that installs an unverified download is worse than none. It resolves symlinks first, so the `git-remote-gdrive` link keeps pointing at the updated file.

After a successful command, the CLI prints a one-line upgrade hint if a newer release exists. It's deliberately unobtrusive:

- Checks at most **once an hour**, cached in `~/.config/drive-git-remote/update-check.json`.
- Resolves the latest version by following `github.com`'s `/releases/latest` redirect rather than querying `api.github.com`, which is rate limited to 60 anonymous requests per hour per IP and is a separate host that restrictive networks sometimes block. Same host as the downloads: if a machine can install, it can check.
- Writes to **stderr**, never stdout, so piped output stays clean.
- Skipped entirely when stderr isn't a terminal — scripts, cron, and the remote helper never see it.
- Skipped for development builds, whose versions aren't comparable to release tags.
- Any failure is silent. A version check is never worth interrupting work over.
- `DRIVE_GIT_NO_UPDATE_CHECK=1` turns it off.

## Releasing

```sh
git tag v1.0.0 && git push origin v1.0.0
```

That triggers `.github/workflows/release.yml`, which runs the test suite, cross-compiles all five targets with the version embedded via ldflags, and publishes archives plus `checksums.txt` to a GitHub release. `make release-snapshot` does the same build locally without publishing.

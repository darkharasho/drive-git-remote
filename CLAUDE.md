## Project Context

A small Go CLI that uses a private Google Drive folder as a git remote, so personal work scripts and productivity apps can be version-controlled without pushing repos to the org's GitHub. Working folders stay ordinary local git repos, so branches, diffs, and history come free; the CLI handles init/clone/push/pull against a bare repo or incremental bundles stored in Drive. Auth is a personal OAuth desktop flow with a cached token, and pushes are guarded by a lock so multiple machines don't clobber each other.

## Goals

- Store and sync personal git repos through a private Google Drive folder instead of org GitHub
- Support real branches, diffs, and full history by using actual git locally rather than a custom versioning scheme
- Provide init/clone/push/pull commands that feel close to normal git remote usage
- Personal OAuth desktop auth with a cached token, scoped to the user's own Drive
- Safe concurrent updates so pushing from two machines doesn't corrupt the remote
- Ship as a single distributable binary that runs on any of the user's machines
- Stay compatible with normal git remotes so a repo can later move to GitHub with no migration

## Out of scope

- Reimplementing git internals (content-addressed objects, packfiles, merge logic)
- Multi-user collaboration, code review, pull requests, or permissions management
- Any dependency on or integration with org-owned GitHub or infrastructure
- A web UI or hosted service

## Suggested stack

- **Go** — Compiles to a single static binary that runs anywhere with no runtime install, and has an official Google Drive API client
- **Google Drive API (v3)** — Free private storage the user already owns, with file versioning and no org visibility
- **OAuth 2.0 desktop flow with cached token** — Authenticates against the user's personal Drive account rather than org credentials
- **git (CLI shell-out or go-git)** — Reuses real git for branches, diffs, history, and bundle/pack generation instead of rebuilding it

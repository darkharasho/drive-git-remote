#!/bin/sh
# Install drive-git.
#
#   curl -fsSL https://raw.githubusercontent.com/darkharasho/drive-git-remote/main/install.sh | sh
#
# Environment:
#   VERSION   tag to install (default: latest release)
#   PREFIX    install directory (default: ~/.local/bin)
#   NO_HELPER set to skip the git-remote-gdrive symlink

set -eu

REPO="darkharasho/drive-git-remote"
PREFIX="${PREFIX:-$HOME/.local/bin}"
VERSION="${VERSION:-}"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

need curl
need tar

# --- platform -----------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	darwin|linux) ;;
	*) die "unsupported OS: $os (Windows users: download the .zip from https://github.com/$REPO/releases)" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64|amd64) arch=amd64 ;;
	arm64|aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch" ;;
esac

# --- resolve version ----------------------------------------------------
if [ -z "$VERSION" ]; then
	say "Finding the latest release..."
	VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		| head -1)
	[ -n "$VERSION" ] || die "could not determine the latest release; set VERSION=vX.Y.Z to pin one"
fi

archive="drive-git_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading drive-git $VERSION ($os/$arch)..."
# curl's own stderr is suppressed here because we report these two failures
# ourselves, and a bare "curl: (56) ... 404" ahead of the real message is noise.
curl -fsL "$base/$archive" -o "$tmp/$archive" 2>/dev/null \
	|| die "no build for $os/$arch in release $VERSION
  see https://github.com/$REPO/releases"
curl -fsL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null \
	|| die "release $VERSION has no checksums.txt; refusing to install unverified"

# --- verify -------------------------------------------------------------
# A binary you are about to put on your PATH deserves a checksum check.
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
else
	die "need sha256sum or shasum to verify the download"
fi

expected=$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt")
[ -n "$expected" ] || die "no checksum listed for $archive"
if [ "$actual" != "$expected" ]; then
	die "checksum mismatch for $archive
  expected $expected
  got      $actual"
fi
say "Checksum verified."

# --- install ------------------------------------------------------------
tar -xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/drive-git" ] || die "archive did not contain drive-git"

mkdir -p "$PREFIX"
# Install to a temp name and rename, so an in-use binary is replaced atomically.
cp "$tmp/drive-git" "$PREFIX/.drive-git.new"
chmod 0755 "$PREFIX/.drive-git.new"
mv "$PREFIX/.drive-git.new" "$PREFIX/drive-git"
say "Installed $PREFIX/drive-git"

if [ -z "${NO_HELPER:-}" ]; then
	ln -sf "$PREFIX/drive-git" "$PREFIX/git-remote-gdrive"
	say "Installed $PREFIX/git-remote-gdrive (git remote helper)"
fi

# --- next steps ---------------------------------------------------------
say ""
case ":$PATH:" in
	*":$PREFIX:"*) ;;
	*)
		say "$PREFIX is not on your PATH. Add it:"
		say "    export PATH=\"$PREFIX:\$PATH\""
		say ""
		;;
esac

say "Next:"
say "    drive-git setup    # create your own Google OAuth client"
say "    drive-git login    # sign in"
say "    cd ~/some-repo && drive-git init"

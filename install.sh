#!/bin/sh
# Install cloudundo: downloads the release for this computer, checks its
# SHA-256 against the release's checksums.txt, and puts it in ~/.local/bin.
#
#   curl -fsSL https://raw.githubusercontent.com/OWNER/cloudundo/main/install.sh | sh
#
# Settings:
#   CLOUDUNDO_VERSION      a release tag such as v0.1.0 (default: latest)
#   CLOUDUNDO_INSTALL_DIR  where to put it (default: ~/.local/bin)
set -eu

# Set once the repository is public. The script refuses to run until then,
# so it can never download from a repository nobody checked.
REPO="${CLOUDUNDO_REPO:-OWNER/cloudundo}"
VERSION="${CLOUDUNDO_VERSION:-latest}"
DIR="${CLOUDUNDO_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "cloudundo install: $*" >&2; exit 1; }

case "$REPO" in OWNER/*) fail "this copy of install.sh has no repository set (CLOUDUNDO_REPO)";; esac

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported system $(uname -s); on Windows, use WSL or download cloudundo-windows-amd64.exe from the releases page" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported processor $(uname -m)" ;;
esac
file="cloudundo-$os-$arch"

if [ -n "${CLOUDUNDO_BASE_URL:-}" ]; then
  base="$CLOUDUNDO_BASE_URL"
elif [ "$VERSION" = latest ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

if command -v curl >/dev/null 2>&1; then
  case "$base" in
    https://*) fetch() { curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"; } ;;
    *) fetch() { curl -fsSL -o "$2" "$1"; } ;; # only for a local mirror via CLOUDUNDO_BASE_URL
  esac
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  fail "needs curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sum() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sum() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "needs sha256sum or shasum to check the download"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading $file ($VERSION)..."
fetch "$base/$file" "$tmp/$file" || fail "could not download $base/$file"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download $base/checksums.txt"

want="$(awk -v f="$file" '$2 == f || $2 == "*"f {print $1}' "$tmp/checksums.txt")"
[ -n "$want" ] || fail "checksums.txt has no entry for $file"
got="$(sum "$tmp/$file")"
[ "$got" = "$want" ] || fail "checksum mismatch for $file (got $got, want $want); not installed"

mkdir -p "$DIR"
chmod 0755 "$tmp/$file"
mv "$tmp/$file" "$DIR/cloudundo"
echo "Installed $("$DIR/cloudundo" version) to $DIR/cloudundo"

case ":$PATH:" in
  *":$DIR:"*) ;;
  *) echo "Add $DIR to your PATH, for example:  echo 'export PATH=\"$DIR:\$PATH\"' >> ~/.bashrc" ;;
esac
echo "Next: cloudundo install claude-code   (or cursor, codex)"

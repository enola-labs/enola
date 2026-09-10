#!/bin/sh
# install.sh: install the latest enola release.
# Usage: curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/install.sh | sh
#
# POSIX sh on purpose, with no `set -o pipefail`. A piped script never honours its
# own shebang: the `sh` on the right of the pipe runs it, and on Debian and Ubuntu
# that is dash, which rejects pipefail before the first line of real work.

set -eu

# --- Detect OS ---
OS="$(uname -s)"
case "$OS" in
  Linux)   OS=linux ;;
  Darwin)  OS=darwin ;;
  # Git Bash, MSYS2 and Cygwin report a decorated name. WSL reports Linux and
  # gets the linux build, which is the right one for it.
  MINGW*|MSYS*|CYGWIN*) OS=windows ;;
  *)       echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

# --- Detect arch ---
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)       ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *)            echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

# --- Fetch latest version ---
VERSION="$(curl -fsSL https://api.github.com/repos/enola-labs/enola/releases/latest | grep '"tag_name"' | sed -E 's/.*"v([^"]+)".*/\1/')"
if [ -z "$VERSION" ]; then
  echo "Failed to fetch latest version" >&2
  exit 1
fi

BASE="enola-${VERSION}-${OS}-${ARCH}"
ASSET="${BASE}.tar.gz"
SHASUM="${BASE}.sha256"
URL="https://github.com/enola-labs/enola/releases/download/v${VERSION}/${ASSET}"
SUM_URL="https://github.com/enola-labs/enola/releases/download/v${VERSION}/${SHASUM}"

echo "==> Downloading enola v${VERSION} for ${OS}/${ARCH} ..."

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

if ! curl -fsSL -o "$TMPDIR/$ASSET" "$URL"; then
  echo "No prebuilt binary for ${OS}/${ARCH} in enola v${VERSION}." >&2
  echo "See the available downloads at:" >&2
  echo "  https://github.com/enola-labs/enola/releases/tag/v${VERSION}" >&2
  exit 1
fi
curl -fsSL -o "$TMPDIR/$SHASUM" "$SUM_URL"

echo "==> Verifying checksum ..."
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$TMPDIR" && sha256sum -c "$SHASUM")
else
  (cd "$TMPDIR" && shasum -a 256 -c "$SHASUM")
fi

echo "==> Extracting ..."
tar xzf "$TMPDIR/$ASSET" -C "$TMPDIR"

# --- Install ---
BIN_NAME="${BASE}"
TARGET="enola"
if [ "$OS" = "windows" ]; then
  BIN_NAME="${BIN_NAME}.exe"
  TARGET="enola.exe"
fi

INSTALL_DIR="${ENOLA_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"

# Windows refuses to overwrite a running executable, and a re-run to upgrade
# usually finds the MCP server holding this one. Renaming it is allowed, so move
# it aside first, as `enola upgrade` does.
if [ "$OS" = "windows" ] && [ -e "$INSTALL_DIR/$TARGET" ]; then
  rm -f "$INSTALL_DIR/$TARGET.old" 2>/dev/null || true
  mv "$INSTALL_DIR/$TARGET" "$INSTALL_DIR/$TARGET.old"
fi

install -m 755 "$TMPDIR/$BIN_NAME" "$INSTALL_DIR/$TARGET"

echo "==> enola v${VERSION} installed to $INSTALL_DIR/$TARGET"
echo ""
echo "If \$HOME/.local/bin is not in your PATH, add it:"
echo "  export PATH=\"$HOME/.local/bin:\$PATH\""

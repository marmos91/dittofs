#!/bin/sh
# Install DittoFS (dfs + dfsctl) binaries
# Usage: curl -fsSL https://github.com/marmos91/dittofs/releases/latest/download/install.sh | sh
set -e

REPO="marmos91/dittofs"
INSTALL_DIR="/usr/local/bin"

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  darwin|linux) ;;
  mingw*|msys*|cygwin*) OS="windows" ;;
  *) echo "Error: unsupported OS: $OS"; exit 1 ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  armv7*) ARCH="armv7" ;;
  *) echo "Error: unsupported architecture: $ARCH"; exit 1 ;;
esac

# Map arch for archive name template (goreleaser uses x86_64/arm64/armv7)
case "$ARCH" in
  amd64) ARCHIVE_ARCH="x86_64" ;;
  arm64) ARCHIVE_ARCH="arm64" ;;
  armv7) ARCHIVE_ARCH="armv7" ;;
esac

# Map OS for archive name template (goreleaser uses title case)
case "$OS" in
  linux) ARCHIVE_OS="Linux" ;;
  darwin) ARCHIVE_OS="Darwin" ;;
  windows) ARCHIVE_OS="Windows" ;;
esac

# Get latest version via GitHub redirect (avoids API rate limits)
echo "Detecting latest version..."
LATEST_URL=$(curl -fsSI "https://github.com/${REPO}/releases/latest" 2>/dev/null | tr -d '\r' | awk 'BEGIN { IGNORECASE = 1 } /^location: / { print $2 }' | tail -n 1)
VERSION=$(printf '%s\n' "$LATEST_URL" | sed -E 's#.*/tag/([^/?]+).*#\1#')
if [ -z "$VERSION" ] || [ "$VERSION" = "$LATEST_URL" ]; then
  echo "Error: could not determine latest version"
  exit 1
fi
VERSION_NUM="${VERSION#v}"

echo "Installing DittoFS $VERSION for $OS/$ARCH..."

# Suggest native packages on supported Linux distros
if [ "$OS" = "linux" ]; then
  PKG_URL="https://github.com/${REPO}/releases/download/${VERSION}"
  if command -v dpkg >/dev/null 2>&1; then
    PKG_FILE="dfs_${VERSION_NUM}_${ARCH}.deb"
    PKG_INSTALL="sudo dpkg -i ${PKG_FILE}"
  elif command -v rpm >/dev/null 2>&1; then
    PKG_FILE="dfs_${VERSION_NUM}_${ARCH}.rpm"
    PKG_INSTALL="sudo rpm -i ${PKG_FILE}"
  elif command -v pacman >/dev/null 2>&1; then
    PKG_FILE="dfs_${VERSION_NUM}_${ARCH}.pkg.tar.zst"
    PKG_INSTALL="sudo pacman -U ${PKG_FILE}"
  fi
  if [ -n "${PKG_FILE:-}" ]; then
    echo ""
    echo "Tip: native package available (includes systemd service). Install with:"
    echo "  curl -fsSLO ${PKG_URL}/${PKG_FILE}"
    echo "  ${PKG_INSTALL}"
    echo ""
    echo "Continuing with binary install..."
  fi
fi

# Determine archive format
if [ "$OS" = "windows" ]; then
  EXT="zip"
else
  EXT="tar.gz"
fi

# Create temporary directory
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

# Download checksums first
CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
echo "Downloading checksums..."
curl -sfL -o "$TMP_DIR/checksums.txt" "$CHECKSUMS_URL" || { echo "Error: checksums download failed"; exit 1; }

# Install both dfs and dfsctl
for BINARY in dfs dfsctl; do
  ARCHIVE="${BINARY}_${VERSION_NUM}_${ARCHIVE_OS}_${ARCHIVE_ARCH}.${EXT}"
  URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"

  echo "Downloading ${BINARY}..."
  curl -sfL -o "$TMP_DIR/$ARCHIVE" "$URL" || { echo "Error: download failed for ${BINARY}"; exit 1; }

  # Verify checksum
  echo "Verifying checksum for ${BINARY}..."
  EXPECTED=$(awk -v f="$ARCHIVE" '$2 == f {print $1}' "$TMP_DIR/checksums.txt")
  if [ -z "$EXPECTED" ]; then
    echo "Error: checksum not found for $ARCHIVE"
    exit 1
  fi

  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL=$(sha256sum "$TMP_DIR/$ARCHIVE" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    ACTUAL=$(shasum -a 256 "$TMP_DIR/$ARCHIVE" | awk '{print $1}')
  else
    echo "Error: no sha256 tool found (sha256sum or shasum required)"
    exit 1
  fi

  if [ "$EXPECTED" != "$ACTUAL" ]; then
    echo "Error: checksum mismatch for ${BINARY}"
    echo "  expected: $EXPECTED"
    echo "  actual:   $ACTUAL"
    exit 1
  fi
  echo "Checksum verified."

  # Extract
  EXTRACT_DIR="$TMP_DIR/${BINARY}_extract"
  mkdir -p "$EXTRACT_DIR"
  if [ "$EXT" = "tar.gz" ]; then
    tar xzf "$TMP_DIR/$ARCHIVE" -C "$EXTRACT_DIR"
  else
    unzip -q "$TMP_DIR/$ARCHIVE" -d "$EXTRACT_DIR"
  fi

  # Install binary
  if [ -w "$INSTALL_DIR" ]; then
    install -m 755 "$EXTRACT_DIR/$BINARY" "$INSTALL_DIR/$BINARY"
  else
    echo "Installing ${BINARY} to $INSTALL_DIR (requires sudo)..."
    sudo install -m 755 "$EXTRACT_DIR/$BINARY" "$INSTALL_DIR/$BINARY"
  fi

  echo "${BINARY} installed."
done

echo ""
echo "DittoFS $VERSION installed to $INSTALL_DIR"
echo "  dfs    - Server daemon"
echo "  dfsctl - Client CLI"
echo ""
echo "Get started:"
echo "  dfs init && dfs start"
echo "  dfsctl login --server http://localhost:8080 --username admin"

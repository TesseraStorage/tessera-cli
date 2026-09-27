#!/usr/bin/env bash
set -euo pipefail

# Tessera CLI installer — Linux / macOS.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/TesseraStorage/tessera-cli/main/install.sh | bash
#
# Or with a specific version:
#   curl -fsSL https://raw.githubusercontent.com/TesseraStorage/tessera-cli/main/install.sh | bash -s v1.0.0

REPO="TesseraStorage/tessera-cli"
VERSION="${1:-latest}"
INSTALL_DIR="${TESSERA_INSTALL_DIR:-/usr/local/bin}"

# Detect OS + arch
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$OS" in
  linux)   GOOS="linux" ;;
  darwin)  GOOS="darwin" ;;
  *)
    echo "Unsupported OS: $OS. This installer works on Linux and macOS."
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64|amd64)  GOARCH="amd64" ;;
  arm64|aarch64)  GOARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

BINARY="tessera-${GOOS}-${GOARCH}"
URL="https://github.com/${REPO}/releases/latest/download/${BINARY}"

if [ "$VERSION" != "latest" ]; then
  URL="https://github.com/${REPO}/releases/download/${VERSION}/${BINARY}"
fi

echo "→ Installing Tessera CLI ${VERSION} for ${GOOS}/${GOARCH}"
echo "  from: ${URL}"
echo "  to:   ${INSTALL_DIR}/tessera"
echo

# Download
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$URL" -o "$TMP/tessera"
elif command -v wget >/dev/null 2>&1; then
  wget -q "$URL" -O "$TMP/tessera"
else
  echo "Need curl or wget. Install one and retry."
  exit 1
fi

chmod +x "$TMP/tessera"

# Install
if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP/tessera" "$INSTALL_DIR/tessera"
else
  echo "Need sudo to write to ${INSTALL_DIR}:"
  sudo mv "$TMP/tessera" "$INSTALL_DIR/tessera"
fi

echo
echo "Tessera CLI installed at ${INSTALL_DIR}/tessera"
echo "Run 'tessera login' to get started."
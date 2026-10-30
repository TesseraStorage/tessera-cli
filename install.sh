#!/bin/sh
# Tessera CLI installer — macOS and Linux.
#
# Downloads a tessera-cli release asset over HTTPS, verifies its SHA-256
# against the release's published SHA256SUMS, and installs it into a
# user-writable directory. It never uses sudo/root, and it refuses to install
# when the checksum is missing or does not match.
#
#   curl -fsSL https://tessera.storage/install.sh | sh
#   curl -fsSL https://tessera.storage/install.sh | sh -s v1.11.4   # a specific version
#
# PATH: the binary goes to ~/.local/bin. If that directory is not on your
# PATH, the installer adds one line to your shell's startup file (~/.zshrc,
# ~/.bashrc or ~/.bash_profile, fish's config.fish, else ~/.profile): it asks
# first when it can reach your terminal, and does it automatically (saying
# which file it changed) when it runs without one, e.g. under an agent or CI.
# Set TESSERA_NO_MODIFY_PATH=1 to leave startup files alone.
#
# Network: installing needs HTTPS to github.com. Using Tessera afterwards needs
# HTTPS to tessera.storage plus outbound TCP to the storage hosts (usually
# port 9984; some hosts announce other ports).
#
# Environment overrides (mainly for tests, forks and air-gapped mirrors):
#   TESSERA_INSTALL_DIR       install directory (default ~/.local/bin)
#   TESSERA_NO_MODIFY_PATH=1  never edit shell startup files
#   TESSERA_RELEASE_REPO      GitHub owner/repo to download from instead of
#                             TesseraStorage/tessera-cli (a fork or mirror;
#                             prints a warning, since the checksum then only
#                             proves the file matches THAT repo's release)
#   TESSERA_RELEASE_BASE_URL  base URL holding the asset + SHA256SUMS
#                             (overrides TESSERA_RELEASE_REPO and the version)
set -eu

OFFICIAL_REPO="TesseraStorage/tessera-cli"
REPO="${TESSERA_RELEASE_REPO:-$OFFICIAL_REPO}"
VERSION="${1:-latest}"
if [ "$VERSION" = latest ]; then
  DEFAULT_BASE="https://github.com/$REPO/releases/latest/download"
else
  DEFAULT_BASE="https://github.com/$REPO/releases/download/$VERSION"
fi
BASE="${TESSERA_RELEASE_BASE_URL:-$DEFAULT_BASE}"
DEST="${TESSERA_INSTALL_DIR:-$HOME/.local/bin}"

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
warn() { printf 'install: warning: %s\n' "$*" >&2; }

if [ "$REPO" != "$OFFICIAL_REPO" ]; then
  warn "TESSERA_RELEASE_REPO is set: downloading from github.com/$REPO, not the official $OFFICIAL_REPO."
  warn "Only continue if you trust that repository."
fi
if [ -n "${TESSERA_RELEASE_BASE_URL:-}" ]; then
  warn "TESSERA_RELEASE_BASE_URL is set: downloading from $BASE."
fi

os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
  Linux)  os_part=linux ;;
  Darwin) os_part=darwin ;;
  *) die "unsupported OS: $os (on Windows use install.ps1)" ;;
esac
case "$arch" in
  x86_64|amd64)  arch_part=amd64 ;;
  arm64|aarch64) arch_part=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac
if [ "$os_part" = linux ] && [ "$arch_part" = arm64 ]; then
  die "no Linux arm64 build is published"
fi
asset="tessera-${os_part}-${arch_part}"

command -v curl >/dev/null 2>&1 || die "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "a SHA-256 tool (sha256sum or shasum) is required"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "[1/4] downloading $asset ($VERSION)"
curl -fsSL "$BASE/$asset" -o "$tmp/$asset" || die "download failed: $BASE/$asset"

echo "[2/4] downloading SHA256SUMS"
curl -fsSL "$BASE/SHA256SUMS" -o "$tmp/SHA256SUMS" || die "download failed: $BASE/SHA256SUMS"

echo "[3/4] verifying checksum"
# SHA256SUMS lines are "<hash>  <name>" or "<hash> *<name>".
expected="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/SHA256SUMS")"
[ -n "$expected" ] || die "no checksum for $asset in SHA256SUMS — refusing to install"
actual="$(sha256 "$tmp/$asset")"
if [ "$expected" != "$actual" ]; then
  die "checksum mismatch for $asset (expected $expected, got $actual) — refusing to install"
fi
echo "      ok: $asset $actual"

echo "[4/4] installing to $DEST/tessera"
mkdir -p "$DEST" || die "cannot create $DEST"
cp "$tmp/$asset" "$DEST/tessera.partial" && chmod 0755 "$DEST/tessera.partial" && mv "$DEST/tessera.partial" "$DEST/tessera" \
  || die "cannot write $DEST/tessera"

installed="$("$DEST/tessera" version 2>/dev/null || echo "tessera (installed)")"
echo "      $installed"

# ---- PATH -------------------------------------------------------------------
path_marker="# added by the Tessera installer"
startup_file() {
  case "$(basename "${SHELL:-sh}")" in
    zsh)  echo "${ZDOTDIR:-$HOME}/.zshrc" ;;
    bash) if [ "$os_part" = darwin ]; then echo "$HOME/.bash_profile"; else echo "$HOME/.bashrc"; fi ;;
    fish) echo "${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish" ;;
    *)    echo "$HOME/.profile" ;;
  esac
}
path_line() {
  case "$1" in
    *config.fish) printf 'fish_add_path "%s" %s\n' "$DEST" "$path_marker" ;;
    *)            printf 'export PATH="%s:$PATH" %s\n' "$DEST" "$path_marker" ;;
  esac
}
add_to_path() {
  rc="$1"
  if [ -f "$rc" ] && grep -F "$DEST" "$rc" 2>/dev/null | grep -qF "$path_marker"; then
    echo "      $rc already adds $DEST to PATH (open a new terminal)"
    return 0
  fi
  mkdir -p "$(dirname "$rc")" 2>/dev/null || true
  if printf '\n%s\n' "$(path_line "$rc")" >>"$rc" 2>/dev/null; then
    echo "      Added $DEST to PATH in $rc (one line, marked \"$path_marker\")."
    echo "      New terminals pick it up; for this one run:  export PATH=\"$DEST:\$PATH\""
  else
    echo "      Could not write $rc. Add it yourself:  export PATH=\"$DEST:\$PATH\""
  fi
}

case ":${PATH:-}:" in
  *":$DEST:"*) ;;
  *)
    rc="$(startup_file)"
    echo
    echo "      $DEST is not on your PATH."
    if [ "${TESSERA_NO_MODIFY_PATH:-}" = 1 ]; then
      echo "      TESSERA_NO_MODIFY_PATH=1, so no startup file was changed. Add it with:"
      echo "        export PATH=\"$DEST:\$PATH\""
    elif (: </dev/tty) 2>/dev/null && [ -t 2 ]; then
      printf '      Add it to PATH in %s? [Y/n] ' "$rc" >/dev/tty
      answer=""
      read -r answer </dev/tty || answer=""
      case "$answer" in
        [nN]*) echo "      Left $rc unchanged. Add it yourself:  export PATH=\"$DEST:\$PATH\"" ;;
        *) add_to_path "$rc" ;;
      esac
    else
      echo "      No terminal to ask, so adding it automatically (set TESSERA_NO_MODIFY_PATH=1 to skip):"
      add_to_path "$rc"
    fi
    echo "      Or run it by full path now: $DEST/tessera"
    ;;
esac
echo
echo "Next: tessera signup   (needs outbound TCP to storage hosts, usually port 9984)"
echo "Done."

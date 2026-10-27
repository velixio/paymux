#!/bin/sh
# Install the paymux binary from a GitHub release.
#
#   curl -fsSL https://github.com/velixio/paymux/releases/latest/download/install.sh | sh
#
# It downloads the archive for this machine (Linux or macOS, amd64 or arm64) and
# the release's SHA256SUMS, checks the checksum (always), checks the cosign
# signature of SHA256SUMS (when cosign is installed), and installs the binary.
# The only thing it runs from the download is the installed binary's `version`.
#
# Settings (environment):
#   PAYMUX_VERSION            install this release (v1.2.3 or 1.2.3) instead of the latest
#   PAYMUX_INSTALL_DIR        where to put paymux (default /usr/local/bin)
#   PAYMUX_USE_SUDO=1         use sudo when the install directory is not writable
#   PAYMUX_REQUIRE_SIGNATURE=1  fail, instead of warning, when the signature cannot be verified
#   PAYMUX_REPO               owner/name of the repository (default velixio/paymux)
#   PAYMUX_DOWNLOAD_BASE      base URL of the releases (default https://github.com/<repo>/releases);
#                             for a mirror, or for tests
#   PAYMUX_COSIGN             the cosign program to use (default: cosign from PATH)
#
# To verify a release by hand instead, see docs/upgrades.md.
set -eu

REPO="${PAYMUX_REPO:-velixio/paymux}"
BASE="${PAYMUX_DOWNLOAD_BASE:-https://github.com/$REPO/releases}"
INSTALL_DIR="${PAYMUX_INSTALL_DIR:-/usr/local/bin}"
COSIGN="${PAYMUX_COSIGN:-cosign}"
OIDC_ISSUER="https://token.actions.githubusercontent.com"

say() { printf '%s\n' "$*"; }
warn() { printf 'install.sh: warning: %s\n' "$*" >&2; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

TMP=""
STAGED=""
cleanup() {
  [ -n "$TMP" ] && rm -rf "$TMP"
  return 0
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

# Everything runs from main, called on the last line: a download cut short
# by "curl | sh" defines nothing and runs nothing.
main() {
  # ---- what to install ----

  case "$(uname -s)" in
    Linux) OS=linux ;;
    Darwin) OS=darwin ;;
    *) die "unsupported operating system: $(uname -s) (Linux and macOS are supported)" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *) die "unsupported CPU architecture: $(uname -m) (amd64 and arm64 are supported)" ;;
  esac

  VERSION="${PAYMUX_VERSION:-}"
  if [ -n "$VERSION" ]; then
    case "$VERSION" in
      *[!A-Za-z0-9._+-]* | -* | .*) die "PAYMUX_VERSION must be a release tag such as v1.2.3" ;;
    esac
    VERSION="v${VERSION#v}"
    ARCHIVE="paymux_${VERSION#v}_${OS}_${ARCH}.tar.gz"
    URL_DIR="$BASE/download/$VERSION"
  else
    # The release's version-less archive: the same file as the versioned one,
    # listed in the same SHA256SUMS.
    ARCHIVE="paymux_${OS}_${ARCH}.tar.gz"
    URL_DIR="$BASE/latest/download"
  fi

  # ---- tools ----

  if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL -o "$2" "$1"; }
  elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
  else
    die "needs curl or wget to download the release"
  fi

  if command -v sha256sum >/dev/null 2>&1; then
    sha256_of() { sha256sum "$1" | awk '{print $1}'; }
  elif command -v shasum >/dev/null 2>&1; then
    sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
  else
    die "needs sha256sum or shasum to check the download"
  fi

  # ---- download ----

  TMP="$(mktemp -d 2>/dev/null || mktemp -d -t paymux-install)"
  say "Downloading $ARCHIVE${VERSION:+ ($VERSION)} ..."
  fetch "$URL_DIR/$ARCHIVE" "$TMP/$ARCHIVE" || die "could not download $URL_DIR/$ARCHIVE"
  fetch "$URL_DIR/SHA256SUMS" "$TMP/SHA256SUMS" || die "could not download $URL_DIR/SHA256SUMS"
  # A release is signed by a Sigstore bundle (cosign v3), or, before that
  # (v0.1.0), by SHA256SUMS.sig with its certificate SHA256SUMS.pem.
  SIGNED=bundle
  if ! fetch "$URL_DIR/SHA256SUMS.sigstore.json" "$TMP/SHA256SUMS.sigstore.json" 2>/dev/null; then
    SIGNED=pair
    fetch "$URL_DIR/SHA256SUMS.sig" "$TMP/SHA256SUMS.sig" 2>/dev/null || SIGNED=none
    fetch "$URL_DIR/SHA256SUMS.pem" "$TMP/SHA256SUMS.pem" 2>/dev/null || SIGNED=none
  fi

  # ---- checksum (always) ----

  want="$(awk -v n="$ARCHIVE" '$2 == n { print $1; exit }' "$TMP/SHA256SUMS")"
  [ -n "$want" ] || die "SHA256SUMS has no entry for $ARCHIVE; refusing to install"
  got="$(sha256_of "$TMP/$ARCHIVE")"
  [ "$got" = "$want" ] || die "checksum mismatch for $ARCHIVE: SHA256SUMS says $want, the download is $got; nothing was installed"
  say "Checksum verified."

  # ---- signature (when cosign is available) ----

  REPO_RE="$(printf '%s' "$REPO" | sed 's/[][\.*^$+?(){}|]/\\&/g')"
  IDENTITY_RE="^https://github\\.com/${REPO_RE}/\\.github/workflows/release\\.yml@refs/tags/v[^/]+\$"
  if [ "$SIGNED" = bundle ]; then
    set -- --bundle "$TMP/SHA256SUMS.sigstore.json" --new-bundle-format
    VERIFY_HELP="to verify it yourself: cosign verify-blob --bundle SHA256SUMS.sigstore.json --new-bundle-format --certificate-identity-regexp '$IDENTITY_RE' --certificate-oidc-issuer $OIDC_ISSUER SHA256SUMS"
  else
    set -- --signature "$TMP/SHA256SUMS.sig" --certificate "$TMP/SHA256SUMS.pem"
    VERIFY_HELP="to verify it yourself: cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig --certificate-identity-regexp '$IDENTITY_RE' --certificate-oidc-issuer $OIDC_ISSUER SHA256SUMS"
  fi
  require="${PAYMUX_REQUIRE_SIGNATURE:-}"

  if [ "$SIGNED" = none ]; then
    [ -z "$require" ] || die "this release is not signed (no SHA256SUMS.sigstore.json, nor SHA256SUMS.sig and SHA256SUMS.pem), and PAYMUX_REQUIRE_SIGNATURE is set; nothing was installed"
    warn "this release is not signed: only the checksum was verified"
  elif ! command -v "$COSIGN" >/dev/null 2>&1; then
    [ -z "$require" ] || die "cosign is not installed, so the signature cannot be verified, and PAYMUX_REQUIRE_SIGNATURE is set; install cosign (https://docs.sigstore.dev/cosign/) and run this again"
    warn "cosign is not installed: the signature was not verified, only the checksum (which comes from the same release)"
    warn "$VERIFY_HELP"
  else
    if "$COSIGN" verify-blob "$@" \
      --certificate-identity-regexp "$IDENTITY_RE" --certificate-oidc-issuer "$OIDC_ISSUER" "$TMP/SHA256SUMS" >"$TMP/cosign.out" 2>&1; then
      say "Signature verified with cosign."
    else
      cat "$TMP/cosign.out" >&2
      die "the SHA256SUMS signature did not verify; nothing was installed"
    fi
  fi

  # ---- unpack and try the binary ----

  mkdir "$TMP/x"
  tar -xzf "$TMP/$ARCHIVE" -C "$TMP/x" paymux || die "$ARCHIVE does not contain a paymux binary"
  chmod 755 "$TMP/x/paymux"
  reported="$("$TMP/x/paymux" version 2>/dev/null)" || die "the downloaded binary does not run on this machine"
  case "$reported" in
    paymux\ *) ;;
    *) die "the downloaded binary answered '$reported' to 'version'; nothing was installed" ;;
  esac
  if [ -n "$VERSION" ] && [ "${reported#paymux }" != "$VERSION" ] && [ "${reported#paymux v}" != "${VERSION#v}" ]; then
    die "the binary reports '$reported' but $VERSION was asked for; nothing was installed"
  fi

  # ---- install ----

  SUDO=""
  if [ ! -d "$INSTALL_DIR" ]; then
    mkdir -p "$INSTALL_DIR" 2>/dev/null || true
  fi
  if [ ! -d "$INSTALL_DIR" ] || [ ! -w "$INSTALL_DIR" ]; then
    if [ "${PAYMUX_USE_SUDO:-}" = 1 ] && command -v sudo >/dev/null 2>&1; then
      SUDO="sudo"
      say "$INSTALL_DIR is not writable by you: using sudo."
    else
      die "cannot write to $INSTALL_DIR. Install somewhere you can write with PAYMUX_INSTALL_DIR=\$HOME/.local/bin, or set PAYMUX_USE_SUDO=1 to use sudo"
    fi
  fi
  [ -z "$SUDO" ] || $SUDO mkdir -p "$INSTALL_DIR"
  STAGED="$INSTALL_DIR/.paymux-install.$$"
  # Copy next to the target and rename, so a running paymux is never half-written.
  if $SUDO cp "$TMP/x/paymux" "$STAGED" && $SUDO chmod 755 "$STAGED" && $SUDO mv -f "$STAGED" "$INSTALL_DIR/paymux"; then
    :
  else
    $SUDO rm -f "$STAGED" 2>/dev/null || true
    die "could not install into $INSTALL_DIR"
  fi

  say "Installed $reported to $INSTALL_DIR/paymux"
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) say "Note: $INSTALL_DIR is not on your PATH." ;;
  esac
  say "Next: set PAYMUX_MASTER_KEY and run 'paymux'; see https://github.com/$REPO#-quick-start-in-5-minutes"
}

main "$@"

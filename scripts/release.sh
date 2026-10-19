#!/usr/bin/env bash
# Build the release artifacts: one tar.gz per platform with the paymux binary
# (static, CGO_ENABLED=0, version stamped in), a SHA256SUMS file and, when
# cosign is installed, a signature over SHA256SUMS. No goreleaser needed.
#
#   scripts/release.sh v1.2.3          # version = the tag; default: git describe
#   OUT=dist/release scripts/release.sh v1.2.3
#   TARGETS="linux/amd64" scripts/release.sh v1.2.3
#
# Environment:
#   OUT        output directory (default dist/release/<version>)
#   TARGETS    space-separated os/arch list (default: linux and darwin, amd64 and arm64)
#   COSIGN_KEY sign with this key instead of keyless (OIDC) signing
#   PAYMUX_NO_COSIGN=1  never sign, even if cosign is installed
#
# Verify a download:
#   sha256sum -c --ignore-missing SHA256SUMS
#   cosign verify-blob --certificate SHA256SUMS.pem --signature SHA256SUMS.sig \
#     --certificate-identity-regexp '^https://github.com/<owner>/<repo>/' \
#     --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
set -euo pipefail
cd "$(dirname "$0")/.."

SUPPORTED="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"
MODULE=github.com/paymux/paymux

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
if ! [[ "$VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]*$ ]]; then
  echo "release: version '$VERSION' must be a plain tag such as v1.2.3" >&2
  exit 2
fi
TARGETS="${TARGETS:-$SUPPORTED}"
for t in $TARGETS; do
  case " $SUPPORTED " in
    *" $t "*) ;;
    *) echo "release: unsupported target '$t' (supported: $SUPPORTED)" >&2; exit 2 ;;
  esac
done
bun install --cwd frontend --frozen-lockfile
bun run --cwd frontend typecheck
bun run --cwd frontend build

OUT="${OUT:-dist/release/$VERSION}"
rm -rf "$OUT"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

# Reproducible archives: fixed timestamps (the commit's), no owner information.
EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}"
TAR_OPTS=()
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
  TAR_OPTS=(--sort=name --owner=0 --group=0 --numeric-owner "--mtime=@${EPOCH}")
fi

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

for t in $TARGETS; do
  os="${t%/*}"
  arch="${t#*/}"
  name="paymux_${VERSION#v}_${os}_${arch}"
  echo "==> build $name"
  mkdir -p "$stage/$name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X ${MODULE}/pkg/hub.Version=${VERSION}" \
    -o "$stage/$name/paymux" ./cmd/paymux
  cp LICENSE NOTICE "$stage/$name/" 2>/dev/null || true
  # Entries are at the top level of the archive: paymux, LICENSE, NOTICE.
  (cd "$stage/$name" && tar "${TAR_OPTS[@]}" -czf "$OUT/$name.tar.gz" paymux $(ls LICENSE NOTICE 2>/dev/null))
done

echo "==> SHA256SUMS"
(
  cd "$OUT"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum ./*.tar.gz
  else
    shasum -a 256 ./*.tar.gz
  fi | sed 's#  \./#  #' > SHA256SUMS
  cat SHA256SUMS
)

echo "==> cosign"
if [ -n "${PAYMUX_NO_COSIGN:-}" ]; then
  echo "cosign: disabled by PAYMUX_NO_COSIGN, not signing"
elif ! command -v cosign >/dev/null 2>&1; then
  echo "cosign: not installed, skipping the signature (SHA256SUMS is still written)"
elif [ -n "${COSIGN_KEY:-}" ]; then
  echo "cosign: signing SHA256SUMS with the key in COSIGN_KEY"
  (cd "$OUT" && cosign sign-blob --yes --key "$COSIGN_KEY" --output-signature SHA256SUMS.sig SHA256SUMS)
else
  echo "cosign: signing SHA256SUMS (keyless)"
  (cd "$OUT" && cosign sign-blob --yes --output-signature SHA256SUMS.sig --output-certificate SHA256SUMS.pem SHA256SUMS)
fi

echo "==> done: $OUT"

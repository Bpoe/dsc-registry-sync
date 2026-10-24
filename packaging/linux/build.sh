#!/usr/bin/env bash
set -euo pipefail
: "${VERSION:?Set VERSION to vMAJOR.MINOR.PATCH}"
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "Expected a stable vMAJOR.MINOR.PATCH version" >&2
    exit 1
}
export GOARCH="${GOARCH:-amd64}"
case "$GOARCH" in
    amd64|arm64) ;;
    *) echo "Unsupported GOARCH: expected amd64 or arm64" >&2; exit 1 ;;
esac
output="$(realpath -m "${1:-dist}")"
mkdir -p "$output"
stage="$(mktemp -d)"
trap 'rm -f "$stage/dscregsyncd"; rmdir "$stage"' EXIT
export BINARY_PATH="$stage/dscregsyncd"
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-X main.version=$VERSION" -o "$BINARY_PATH" ./cmd/dscregsyncd
for format in deb rpm; do
    nfpm package --config packaging/linux/nfpm.yaml --packager "$format" --target "$output/"
done

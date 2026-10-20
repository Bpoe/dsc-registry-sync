#!/usr/bin/env bash
set -euo pipefail
: "${VERSION:?Set VERSION to vMAJOR.MINOR.PATCH}"
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "Expected a stable vMAJOR.MINOR.PATCH version" >&2
    exit 1
}
output="$(realpath -m "${1:-dist}")"
mkdir -p "$output"
stage="$(mktemp -d)"
trap 'rm -f "$stage/dscregsyncd"; rmdir "$stage"' EXIT
export BINARY_PATH="$stage/dscregsyncd"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$VERSION" -o "$BINARY_PATH" ./cmd/dscregsyncd
for format in deb rpm; do
    nfpm package --config packaging/linux/nfpm.yaml --packager "$format" --target "$output/"
done

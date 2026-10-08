#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
: "${VERSION:?Set VERSION to a release tag, for example v0.0.1-rc.1}"
if [[ ! $VERSION =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.[1-9][0-9]*)?$ ]]; then
    echo "VERSION must be vX.Y.Z or vX.Y.Z-rc.N" >&2
    exit 1
fi
output=${1:-dist}
mkdir -p "$output"
output=$(realpath "$output")
export STAGE_DIR="$output/.stage"
mkdir -p "$STAGE_DIR"
trap 'rm -rf "$STAGE_DIR"' EXIT
TMPDIR="$STAGE_DIR" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$VERSION" -o "$STAGE_DIR/dscd" ./cmd/dscd
for format in deb rpm; do
    nfpm package --config packaging/linux/nfpm.yaml --packager "$format" --target "$output/"
done

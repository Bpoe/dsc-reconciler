#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
: "${VERSION:?Set VERSION to a release tag, for example v0.0.3}"
if [[ ! $VERSION =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo "VERSION must be vX.Y.Z" >&2
    exit 1
fi
output=${1:-dist}
mkdir -p "$output"
output=$(realpath "$output")
export STAGE_DIR="$output/.stage"
mkdir "$STAGE_DIR"
trap 'rm -rf "$STAGE_DIR"' EXIT
TMPDIR="$STAGE_DIR" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$VERSION" -o "$STAGE_DIR/dscd" ./cmd/dscd
[[ $("$STAGE_DIR/dscd" --version) == "dscd $VERSION" ]]
"$STAGE_DIR/dscd" -help
for format in deb rpm; do
    nfpm package --config packaging/linux/nfpm.yaml --packager "$format" --target "$output/"
done
name="dscd-$VERSION-linux-amd64"
package="$STAGE_DIR/$name"
mkdir -p "$package/bin" "$package/docs" "$package/packaging"
cp "$STAGE_DIR/dscd" "$package/bin/"
cp README.md LICENSE AGENTS.md "$package/"
cp docs/design.md "$package/docs/"
cp -R packaging/systemd "$package/packaging/"
tar -czf "$output/$name.tar.gz" -C "$STAGE_DIR" "$name"
(cd "$output" && sha256sum "$name.tar.gz" "dscd_${VERSION#v}_amd64.deb" \
    "dscd-${VERSION#v}-1.x86_64.rpm" > SHA256SUMS)

#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
: "${VERSION:?Set VERSION to a release tag, for example v0.0.3}"
if [[ ! $VERSION =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo "VERSION must be vX.Y.Z" >&2
    exit 1
fi
case "$(uname -s):$(uname -m)" in
    Linux:x86_64) native_arch=amd64 ;;
    Linux:aarch64) native_arch=arm64 ;;
    *) echo "Packaging requires native Linux AMD64 or ARM64" >&2; exit 1 ;;
esac
export ARCH=${ARCH:-$native_arch}
[[ $ARCH == "$native_arch" ]] || { echo "ARCH must match native architecture $native_arch" >&2; exit 1; }
case "$ARCH" in
    amd64) rpm_arch=x86_64 ;;
    arm64) rpm_arch=aarch64 ;;
esac
output=${1:-dist}
mkdir -p "$output"
output=$(realpath "$output")
export STAGE_DIR="$output/.stage"
mkdir "$STAGE_DIR"
trap 'rm -rf "$STAGE_DIR"' EXIT
TMPDIR="$STAGE_DIR" CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "-X main.version=$VERSION" -o "$STAGE_DIR/dscd" ./cmd/dscd
[[ $("$STAGE_DIR/dscd" --version) == "dscd $VERSION" ]]
"$STAGE_DIR/dscd" -help
for format in deb rpm; do
    nfpm package --config packaging/linux/nfpm.yaml --packager "$format" --target "$output/"
done
name="dscd-$VERSION-linux-$ARCH"
package="$STAGE_DIR/$name"
mkdir -p "$package/bin" "$package/docs" "$package/packaging"
cp "$STAGE_DIR/dscd" "$package/bin/"
cp README.md LICENSE AGENTS.md "$package/"
cp docs/design.md "$package/docs/"
cp -R packaging/systemd "$package/packaging/"
tar -czf "$output/$name.tar.gz" -C "$STAGE_DIR" "$name"
(cd "$output" && sha256sum "$name.tar.gz" "dscd_${VERSION#v}_${ARCH}.deb" \
    "dscd-${VERSION#v}-1.$rpm_arch.rpm" > SHA256SUMS)

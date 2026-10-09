#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
: "${VERSION:?Set VERSION to the tag used to build the packages}"
[[ $# -eq 1 ]] || { echo "Usage: VERSION=vX.Y.Z bash $0 OUTPUT_DIR" >&2; exit 1; }
output=$(realpath "$1")
version=${VERSION#v}
[[ $VERSION =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
deb="$output/dscd_${version}_amd64.deb"
rpm="$output/dscd-${version}-1.x86_64.rpm"
archive="$output/dscd-$VERSION-linux-amd64.tar.gz"
[[ $(dpkg-deb -f "$deb" Package) == dscd ]]
[[ $(dpkg-deb -f "$deb" Version) == "$version" ]]
[[ $(dpkg-deb -f "$deb" Architecture) == amd64 ]]
dpkg-deb -f "$deb" Depends | grep -F 'dsc (>= 3.3.0)'
[[ $(rpm -qp --qf '%{NAME} %{VERSION} %{RELEASE} %{ARCH}' "$rpm") == "dscd $version 1 x86_64" ]]
rpm -qp --requires "$rpm" | grep -Fx 'dsc >= 3.3.0'
rpm --checksig "$rpm"

stage="$(dirname "$deb")/.verify"
mkdir "$stage"
trap 'rm -rf "$stage"' EXIT
tar -xzf "$archive" -C "$stage"
archive_root="$stage/dscd-$VERSION-linux-amd64"
[[ $("$archive_root/bin/dscd" --version) == "dscd $VERSION" ]]
for file in README.md LICENSE AGENTS.md docs/design.md packaging/systemd/dscd.service; do
    cmp "$file" "$archive_root/$file"
done
dpkg-deb --extract "$deb" "$stage/deb"
mkdir "$stage/rpm"
rpm2archive -n < "$rpm" > "$stage/rpm.tar"
tar -xf "$stage/rpm.tar" -C "$stage/rpm"
for format in deb rpm; do
    root="$stage/$format"
    [[ $(stat -c %a "$root/usr/bin/dscd") == 755 ]]
    cmp "$archive_root/bin/dscd" "$root/usr/bin/dscd"
    [[ $(stat -c %a "$root/usr/lib/systemd/system/dscd.service") == 644 ]]
    cmp packaging/systemd/dscd.service "$root/usr/lib/systemd/system/dscd.service"
    cmp LICENSE "$root/usr/share/licenses/dscd/LICENSE"
    "$root/usr/bin/dscd" -help
    [[ $("$root/usr/bin/dscd" --version) == "dscd $VERSION" ]]
    grep -Fx 'ExecStart=/usr/bin/dscd -config-dir /etc/dsc/config.d -results-dir /var/lib/dsc/results.d -dsc-path /usr/bin/dsc' "$root/usr/lib/systemd/system/dscd.service"
    # Supply only system targets in the extracted root; never install onto the host.
    for target in sysinit basic shutdown local-fs multi-user; do
        printf '[Unit]\nDescription=Verification target\n' > "$root/usr/lib/systemd/system/$target.target"
    done
    systemd-analyze verify --root="$root" /usr/lib/systemd/system/dscd.service
done
dpkg-deb --contents "$deb" | awk '$2 != "root/root" { bad=1 } END { exit bad }'
rpm -qp --qf '[%{FILEUSERNAME} %{FILEGROUPNAME}\n]' "$rpm" |
    awk '$0 != "root root" { bad=1 } END { exit bad }'
echo "Archive, package metadata, identical executable payloads and units verified."

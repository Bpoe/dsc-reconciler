#!/usr/bin/env bash
# Destructive integration check: run only as root on a disposable machine/container.
set -euo pipefail

[[ $EUID -eq 0 && $# -eq 2 ]] || {
    echo "Usage (disposable host only): sudo bash $0 INITIAL_PACKAGE NEWER_PACKAGE" >&2
    exit 1
}
initial=$(realpath "$1")
newer=$(realpath "$2")
[[ $initial != "$newer" && ${initial##*.} == "${newer##*.}" ]]
[[ -x /usr/bin/dsc ]]
/usr/bin/dsc --version
[[ ! -e /usr/bin/dscd && ! -e /etc/dsc/config.d && ! -e /var/lib/dsc/results.d ]]
systemd_running=false
if [[ -d /run/systemd/system ]]; then
    systemd_running=true
    trap 'journalctl -u dscd --no-pager -n 40 >&2' ERR
fi

install_package() {
    case "$initial" in
        *.deb) apt-get install --yes "$1" ;;
        *.rpm) dnf install --assumeyes "$1" ;;
        *) echo "Expected DEB or RPM packages" >&2; exit 1 ;;
    esac
}
remove_package() {
    case "$initial" in
        *.deb) apt-get remove --yes dscd ;;
        *.rpm) dnf remove --assumeyes --setopt=clean_requirements_on_remove=False dscd ;;
    esac
}
assert_running() {
    [[ -L /etc/systemd/system/multi-user.target.wants/dscd.service ]]
    if "$systemd_running"; then
        systemctl is-enabled --quiet dscd.service
        systemctl is-active --quiet dscd.service
        [[ $(systemctl show -p User --value dscd.service) == root ]]
        [[ $(systemctl show -p FragmentPath --value dscd.service) == */systemd/system/dscd.service ]]
        [[ $(systemctl show -p MainPID --value dscd.service) -gt 0 ]]
    else
        # An image build must register the unit without launching a foreground daemon.
        for process in /proc/[0-9]*/comm; do
            if IFS= read -r name < "$process"; then
                [[ $name != dscd ]]
            fi
        done
    fi
}
assert_preserved() {
    [[ $(cat /etc/dsc/config.d/.package-smoke.txt) == config ]]
    [[ $(cat /var/lib/dsc/results.d/.package-smoke.txt) == result ]]
    [[ $(stat -c %a /etc/dsc/config.d) == 750 ]]
    [[ $(stat -c %a /var/lib/dsc/results.d) == 710 ]]
    [[ $(stat -c %a /etc/dsc/config.d/.package-smoke.txt) == 640 ]]
}
assert_removed() {
    [[ ! -e /etc/systemd/system/multi-user.target.wants/dscd.service &&
       ! -L /etc/systemd/system/multi-user.target.wants/dscd.service ]]
    if "$systemd_running" && systemctl is-active --quiet dscd.service; then exit 1; fi
    [[ ! -e /usr/bin/dscd && ! -e /usr/lib/systemd/system/dscd.service ]]
    [[ -x /usr/bin/dsc ]]
    assert_preserved
}

install_package "$initial"
assert_running
for directory in /etc/dsc/config.d /var/lib/dsc/results.d; do
    [[ $(stat -c '%U:%G %a' "$directory") == 'root:root 700' ]]
done
printf config > /etc/dsc/config.d/.package-smoke.txt
printf result > /var/lib/dsc/results.d/.package-smoke.txt
chmod 750 /etc/dsc/config.d
chmod 710 /var/lib/dsc/results.d
chmod 640 /etc/dsc/config.d/.package-smoke.txt
if "$systemd_running"; then
    systemctl stop dscd.service
    if systemctl is-active --quiet dscd.service; then exit 1; fi
    systemctl start dscd.service
    assert_running
    old_pid=$(systemctl show -p MainPID --value dscd.service)
fi
install_package "$newer"
assert_running
if "$systemd_running"; then
    [[ $(systemctl show -p MainPID --value dscd.service) != "$old_pid" ]]
fi
assert_preserved
case "$initial" in
    *.deb) apt-get install --yes --reinstall "$newer" ;;
    *.rpm) dnf reinstall --assumeyes "$newer" ;;
esac
assert_running
assert_preserved
remove_package
assert_removed

# Reinstall with preserved data, then verify upgrading an enabled-but-stopped unit.
install_package "$initial"
assert_running
assert_preserved
if "$systemd_running"; then systemctl stop dscd.service; fi
install_package "$newer"
if "$systemd_running" && systemctl is-active --quiet dscd.service; then exit 1; fi
[[ -L /etc/systemd/system/multi-user.target.wants/dscd.service ]]
assert_preserved
remove_package
assert_removed
if [[ $initial == *.deb ]]; then
    apt-get purge --yes dscd
    assert_removed
fi
if "$systemd_running"; then
    echo "Install, restart, running/stopped upgrades, reinstall, removal and data preservation passed."
else
    echo "No-systemd installation, upgrade, reinstall, removal and data preservation passed; service startup NOT tested."
fi

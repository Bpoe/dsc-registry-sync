#!/usr/bin/env bash
set -euo pipefail
[[ "${DSCREGSYNC_DISPOSABLE:-}" == 1 && "$EUID" == 0 ]] || {
    echo "Requires root and DSCREGSYNC_DISPOSABLE=1 on a disposable host" >&2
    exit 1
}
package="$(realpath "$1")"
upgrade="${2:-}"
if command -v dscregsyncd >/dev/null || [[ -e /etc/dsc/dscregsyncd.json || -e /etc/dsc/dscregsyncd.key ||
    -e /etc/dsc/config.d/dscregsync || -e /var/lib/dsc/results.d/dscregsync ]]; then
    echo "Refusing to touch an existing installation" >&2
    exit 1
fi
install -d -m 0700 /etc/dsc/config.d/dscregsync /var/lib/dsc/results.d/dscregsync
umask 077
printf '%s' '{"registryUrl":"https://127.0.0.1:9","requestTimeoutSeconds":1}' > /etc/dsc/dscregsyncd.json
printf 'dscm_%s.%s' "$(cat /proc/sys/kernel/random/uuid)" "$(cat /proc/sys/kernel/random/uuid)" > /etc/dsc/dscregsyncd.key
printf '{}' > /etc/dsc/config.d/dscregsync/smoke.json
printf '{}' > /etc/dsc/config.d/dscregsync/smoke.parameters.json
printf '%s' 11111111-1111-4111-8111-111111111111 > /etc/dsc/config.d/dscregsync/.smoke.revision
printf '"preserved"' > /etc/dsc/config.d/dscregsync/machine.etag
printf '{}' > /var/lib/dsc/results.d/dscregsync/smoke.json.result.json
manifest="$(mktemp)"
files=(/etc/dsc/dscregsyncd.json /etc/dsc/dscregsyncd.key
    /etc/dsc/config.d/dscregsync/smoke.json /etc/dsc/config.d/dscregsync/smoke.parameters.json
    /etc/dsc/config.d/dscregsync/.smoke.revision /etc/dsc/config.d/dscregsync/machine.etag
    /var/lib/dsc/results.d/dscregsync/smoke.json.result.json)
sha256sum "${files[@]}" > "$manifest"
installed=0
remove_package() {
    if [[ "$package" == *.deb ]]; then dpkg --purge dscregsyncd; else rpm -e dscregsyncd; fi
}
cleanup() {
    status=$?
    if [[ "$status" != 0 && -d /run/systemd/system ]]; then
        systemctl show dscregsyncd --property=ActiveState,SubState,Result,NRestarts
        journalctl -u dscregsyncd --no-pager -n 80
    fi
    if [[ "$installed" == 1 ]]; then remove_package; fi
    rm -f "$manifest" "${files[@]}"
    rmdir /etc/dsc/config.d/dscregsync /var/lib/dsc/results.d/dscregsync
}
trap cleanup EXIT
install_package() {
    if [[ "$1" == *.deb ]]; then dpkg --install "$1"; else rpm -U --replacepkgs "$1"; fi
}
check() {
    if [[ "$package" == *.deb ]]; then
        version="$(dpkg-query -W -f='${Version}' dscregsyncd)"
    else
        version="$(rpm -q --qf '%{VERSION}' dscregsyncd)"
    fi
    [[ "$(dscregsyncd -version 2>&1)" == "dscregsyncd v$version" ]]
    sha256sum --check --status "$manifest"
    for file in "${files[@]}"; do
        [[ "$(stat -c '%a:%U:%G' "$file")" == 600:root:root ]]
    done
    if [[ -d /run/systemd/system ]]; then
        systemctl is-enabled --quiet dscregsyncd
        systemctl start dscregsyncd
        systemctl is-active --quiet dscregsyncd
        systemctl restart dscregsyncd
        systemctl is-active --quiet dscregsyncd
        systemctl stop dscregsyncd
        ! systemctl is-active --quiet dscregsyncd
        [[ "$(systemctl show dscregsyncd --property=Result --value)" == success ]]
        [[ "$(systemctl show dscregsyncd --property=NRestarts --value)" == 0 ]]
        # Separate clean lifecycle scenarios must not share systemd's start counter.
        systemctl reset-failed dscregsyncd
    fi
    sha256sum --check --status "$manifest"
}
install_package "$package"
installed=1
check
install_package "$package"
if [[ -d /run/systemd/system ]]; then
    ! systemctl is-active --quiet dscregsyncd
fi
check
if [[ -n "$upgrade" ]]; then
    if [[ -d /run/systemd/system ]]; then systemctl start dscregsyncd; fi
    install_package "$(realpath "$upgrade")"
    if [[ -d /run/systemd/system ]]; then systemctl is-active --quiet dscregsyncd; fi
    check
fi
remove_package
installed=0
sha256sum --check --status "$manifest"
[[ ! -e /usr/bin/dscregsyncd && ! -e /usr/lib/systemd/system/dscregsyncd.service ]]

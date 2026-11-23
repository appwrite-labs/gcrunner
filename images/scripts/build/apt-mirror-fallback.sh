#!/bin/sh
set -eu

sources=/etc/apt/sources.list.d/ubuntu.sources
[ -f "$sources" ] || exit 0
archive=http://archive.ubuntu.com/ubuntu/
security=http://security.ubuntu.com/ubuntu/
lists=/etc/apt/mirrors
uri=$(awk '/^URIs:/ {print $2; exit}' "$sources")
case "$uri" in
    mirror+file:/etc/apt/mirrors/ubuntu.list)
        # A second invocation can follow the previous image's boot service.
        regional=$(awk '$1 ~ /^https?:\/\/[^/]+\.gce\.archive\.ubuntu\.com\// {print $1; exit}' "$lists/ubuntu.list")
        ;;
    http://*.gce.archive.ubuntu.com/*|https://*.gce.archive.ubuntu.com/*) regional=$uri ;;
    *) exit 0 ;;
esac
[ -n "$regional" ] || exit 0
# shellcheck source=/dev/null
. /etc/os-release
mkdir -p "$lists"
tmp=$(mktemp -d "$lists/.probe.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

responds() {
    curl -fsSL --max-time 3 --output "$tmp/$2.InRelease" "${1%/}/dists/$2/InRelease" &&
        gpgv --keyring /usr/share/keyrings/ubuntu-archive-keyring.gpg "$tmp/$2.InRelease" >/dev/null 2>&1
}

order_mirrors() {
    primary=$1
    fallback=$regional
    if ! responds "$primary" "$2" && responds "$regional" "$2"; then
        primary=$regional
        fallback=$1
    fi
    printf '%s\tpriority:1\n%s\tpriority:2\n' "$primary" "$fallback" > "$tmp/$3"
    chmod 644 "$tmp/$3"
    mv "$tmp/$3" "$lists/$3"
}

# Probe concurrently before the runner starts. Apt retries files individually,
# so giving it an unresponsive primary can add a minute for every index.
order_mirrors "$archive" "$VERSION_CODENAME-updates" ubuntu.list &
archive_pid=$!
order_mirrors "$security" "$VERSION_CODENAME-security" security.list &
security_pid=$!
wait "$archive_pid"
wait "$security_pid"

# Keep the security pocket separate and preserve Signed-By and the suites.
sed -i \
    -e '0,/^URIs:/s|^URIs:.*|URIs: mirror+file:/etc/apt/mirrors/ubuntu.list|' \
    -e 's|^URIs: https\?://security\.ubuntu\.com/ubuntu/*$|URIs: mirror+file:/etc/apt/mirrors/security.list|' \
    "$sources"

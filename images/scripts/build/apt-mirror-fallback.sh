#!/bin/sh
# Serve apt from the canonical Ubuntu mirrors with the region's GCE mirror as
# fallback, and put the GCE mirror first for a pocket whose canonical mirror
# does not serve signed metadata within three seconds. Apt retries each file on
# an unresponsive first mirror before falling back, which adds about a minute
# per index. Rewrites cloud-init's sources, so it must run after cloud-config.
set -eu

# shellcheck source=/dev/null
. /etc/os-release
keyring=/usr/share/keyrings/ubuntu-archive-keyring.gpg
zone=$(curl -fsS --max-time 3 -H 'Metadata-Flavor: Google' \
    http://metadata.google.internal/computeMetadata/v1/instance/zone)
zone=${zone##*/}
regional=http://${zone%-*}.gce.archive.ubuntu.com/ubuntu/

# Succeeds when the mirror serves the suite's InRelease, signed by Ubuntu.
serves() {
    release=$(mktemp)
    curl -fsS --max-time 3 -o "$release" "$1dists/$2/InRelease" &&
        gpgv --keyring "$keyring" "$release" 2>/dev/null
    status=$?
    rm -f "$release"
    return "$status"
}

# Writes an apt mirror list: canonical first unless only the regional mirror serves.
mirrors() {
    first=$1
    second=$regional
    if ! serves "$1" "$2" && serves "$regional" "$2"; then
        first=$regional
        second=$1
    fi
    printf '%s\tpriority:1\n%s\tpriority:2\n' "$first" "$second" > "$3"
}

mkdir -p /etc/apt/mirrors
mirrors http://archive.ubuntu.com/ubuntu/ "$VERSION_CODENAME-updates" /etc/apt/mirrors/ubuntu.list &
mirrors http://security.ubuntu.com/ubuntu/ "$VERSION_CODENAME-security" /etc/apt/mirrors/security.list &
wait

cat > /etc/apt/sources.list.d/ubuntu.sources <<EOF
Types: deb
URIs: mirror+file:/etc/apt/mirrors/ubuntu.list
Suites: $VERSION_CODENAME $VERSION_CODENAME-updates $VERSION_CODENAME-backports
Components: main universe restricted multiverse
Signed-By: $keyring

Types: deb
URIs: mirror+file:/etc/apt/mirrors/security.list
Suites: $VERSION_CODENAME-security
Components: main universe restricted multiverse
Signed-By: $keyring
EOF

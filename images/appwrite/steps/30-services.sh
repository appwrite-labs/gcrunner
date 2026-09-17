#!/bin/bash -e
# A VM that lives for one job has no use for snaps, crash reporting or housekeeping timers.
if [ "$(jq -r '.services.remove_snapd' "${APPWRITE_TOOLSET}")" = "true" ]; then
    for round in 1 2; do
        for snap in $(snap list 2>/dev/null | awk 'NR>1 && $1!="snapd" {print $1}' | sort -r); do
            snap remove --purge "${snap}" || true
        done
    done
    apt-get purge -y snapd
    rm -rf /snap /var/snap /var/lib/snapd /root/snap
fi

for unit in $(jq -r '.services.disable[]' "${APPWRITE_TOOLSET}"); do
    systemctl disable --now "${unit}" 2>/dev/null || true
    systemctl mask "${unit}" 2>/dev/null || true
done

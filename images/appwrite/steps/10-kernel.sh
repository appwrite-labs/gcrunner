#!/bin/bash -e
# MongoDB 8.0 refuses kernels 6.19 to 7.0.13 (SERVER-121912), so pin the newest GCP kernel
# below that range instead of the rolling one. 6.17 (HWE) has io_uring ops 6.8 lacks
# (IORING_OP_FTRUNCATE, 6.9+).
series=$(jq -r '.kernel.series' "${APPWRITE_TOOLSET}")
package=$(jq -r '.kernel.package' "${APPWRITE_TOOLSET}")
rolling=$(jq -r '.kernel.rolling_packages[]' "${APPWRITE_TOOLSET}")

apt-get update
apt-get install -y "${package}"
apt-get purge -y ${rolling} || true

for installed in $(dpkg-query --show --showformat='${Package}\n' 'linux-image-*' 'linux-modules-*' 'linux-headers-*' 2>/dev/null); do
    case "${installed}" in
        *"-gcp-${series}" | *"${series}.0-"*) ;;
        *) apt-get purge -y "${installed}" ;;
    esac
done
apt-get autoremove -y --purge
update-grub

echo "Installed kernels:"
dpkg-query --show --showformat='  ${Package}\n' 'linux-image-*'

#!/bin/bash -e
################################################################################
##  File:  pin-kernel.sh
##  Desc:  Boot Ubuntu 24.04's GA GCP kernel (6.8) instead of the rolling HWE
##         kernel. MongoDB 8.0 refuses kernels 6.19 to 7.0.13 (SERVER-121912).
################################################################################
export DEBIAN_FRONTEND=noninteractive

KERNEL_SERIES="6.8"
KERNEL_PACKAGE="linux-gcp-${KERNEL_SERIES}"
ROLLING_PACKAGES=(linux-gcp linux-image-gcp linux-headers-gcp linux-modules-extra-gcp)

installed_kernel_packages() {
    dpkg-query --show --showformat='${Package}\n' 'linux-image-*' 'linux-modules-*' 'linux-headers-*' 2>/dev/null
}

apt-get update
apt-get install -y "${KERNEL_PACKAGE}"

echo "Removing the rolling kernel meta packages"
apt-get purge -y "${ROLLING_PACKAGES[@]}" || true

echo "Removing every kernel outside the ${KERNEL_SERIES} series"
for package in $(installed_kernel_packages); do
    case "${package}" in
        *"-gcp-${KERNEL_SERIES}" | *"${KERNEL_SERIES}.0-"*) ;;
        *) apt-get purge -y "${package}" ;;
    esac
done
apt-get autoremove -y --purge
update-grub

echo "Installed kernels:"
dpkg-query --show --showformat='  ${Package}\n' 'linux-image-*'

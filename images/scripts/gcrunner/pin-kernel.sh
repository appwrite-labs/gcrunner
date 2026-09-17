#!/bin/bash -e
################################################################################
##  File:  pin-kernel.sh
##  Desc:  Boot the Ubuntu 24.04 GA GCP kernel (6.8) instead of the rolling HWE
##         kernel. MongoDB 8.0 refuses kernels 6.19 to 7.0.13 (SERVER-121912).
################################################################################
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y linux-gcp-6.8
# Drop the rolling meta packages and every kernel that is not the 6.8 series.
apt-get purge -y linux-gcp linux-image-gcp linux-headers-gcp linux-modules-extra-gcp || true
dpkg -l 'linux-image-*' 'linux-modules-*' 'linux-headers-*' 2>/dev/null \
  | awk '/^ii/ && $2 !~ /(^linux-(image|modules|headers)-gcp-6\.8$)|6\.8\.0-/ {print $2}' \
  | xargs -r apt-get purge -y
apt-get autoremove -y --purge
update-grub
echo "installed kernels:"; dpkg -l 'linux-image-*' | awk '/^ii/ {print "  " $2}'

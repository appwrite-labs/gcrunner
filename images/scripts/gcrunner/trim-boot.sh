#!/bin/bash -e
################################################################################
##  File:  trim-boot.sh
##  Desc:  Drop services a throwaway CI VM never uses so it reaches the runner
##         agent sooner.
################################################################################
export DEBIAN_FRONTEND=noninteractive

for snap in $(snap list 2>/dev/null | awk 'NR>1 && $1!="snapd" && $1!~/^core/ {print $1}'); do
    snap remove --purge "${snap}"
done
for snap in $(snap list 2>/dev/null | awk 'NR>1 && $1!="snapd" {print $1}'); do
    snap remove --purge "${snap}"
done
apt-get purge -y snapd
rm -rf /snap /var/snap /var/lib/snapd /root/snap

systemctl disable --now apport.service motd-news.timer man-db.timer \
    update-notifier-download.timer update-notifier-motd.timer e2scrub_all.timer 2>/dev/null || true
systemctl mask apport.service

#!/bin/bash -e
################################################################################
##  File:  configure-apt.sh
##  Desc:  Configure apt, install jq
##  From:  actions/runner-images (MIT License)
################################################################################

source $HELPER_SCRIPTS/os.sh

# Stop and disable apt-daily upgrade services
systemctl stop apt-daily.timer
systemctl disable apt-daily.timer
systemctl disable apt-daily.service
systemctl stop apt-daily-upgrade.timer
systemctl disable apt-daily-upgrade.timer
systemctl disable apt-daily-upgrade.service

# apt reads Acquire::Retries (APT::Acquire::Retries is ignored, leaving the default of 3) and
# tries a file on the next mirror only once its retries are spent. The http method reconnects
# once on its own, so each attempt on a silent server costs two timeouts: one retry at 15s gives
# up on a stalled mirror after about 60s per file, where the defaults took 16 minutes.
echo 'Acquire::Retries "1";' > /etc/apt/apt.conf.d/80-retries
echo 'Acquire::http::Timeout "15";' > /etc/apt/apt.conf.d/80-timeout

# cloud-init writes ubuntu.sources on each new instance; replace it before the runner starts.
install -m 755 /tmp/apt-mirror-fallback.sh /usr/local/sbin/apt-mirror-fallback

cat <<'EOF' > /etc/systemd/system/apt-mirror-fallback.service
[Unit]
Description=Select responsive Ubuntu archive and security mirrors
After=cloud-config.service
Before=google-startup-scripts.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/apt-mirror-fallback

[Install]
WantedBy=multi-user.target
EOF
systemctl enable apt-mirror-fallback.service

# Configure apt to always assume Y
echo "APT::Get::Assume-Yes \"true\";" > /etc/apt/apt.conf.d/90assumeyes

# Always include phased updates
echo 'APT::Get::Always-Include-Phased-Updates "true";' > /etc/apt/apt.conf.d/99-phased-updates

# Fix bad proxy and http headers settings
cat <<EOF >> /etc/apt/apt.conf.d/99bad_proxy
Acquire::http::Pipeline-Depth 0;
Acquire::http::No-Cache true;
Acquire::https::Pipeline-Depth 0;
Acquire::https::No-Cache true;
Acquire::BrokenProxy    true;
EOF

# Uninstall unattended-upgrades
apt-get purge unattended-upgrades || true

apt-get update
apt-get install jq

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

# cloud-init writes the region's GCE mirror into ubuntu.sources on each new instance. Some of
# those mirrors accept the connection and then never answer for certain files, so after
# cloud-init has run, serve that entry through apt's mirror method with archive.ubuntu.com
# behind it: a file the regional mirror fails on is fetched from the archive instead. A unit
# rather than cloud-init's apt.sources, which runs apt-get update on every boot.
cat <<'EOF' > /usr/local/sbin/apt-mirror-fallback
#!/bin/sh -e
sources=/etc/apt/sources.list.d/ubuntu.sources
list=/etc/apt/mirrors/ubuntu.list
mirror=$(awk '/^URIs:/ {print $2; exit}' "$sources")
case "$mirror" in
    "" | mirror+*) exit 0 ;;
esac
mkdir -p "$(dirname "$list")"
printf '%s\tpriority:1\nhttp://archive.ubuntu.com/ubuntu/\tpriority:2\n' "$mirror" > "$list"
sed -i "0,/^URIs:/s|^URIs:.*|URIs: mirror+file:$list|" "$sources"
EOF
chmod +x /usr/local/sbin/apt-mirror-fallback

cat <<'EOF' > /etc/systemd/system/apt-mirror-fallback.service
[Unit]
Description=Fall back to archive.ubuntu.com when the GCE apt mirror fails
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

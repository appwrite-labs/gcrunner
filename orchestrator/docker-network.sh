#!/bin/bash
# Runs before the Actions runner, when restarting Docker cannot interrupt a job.
# Keep this in a subshell so temporary-file cleanup cannot replace startup traps.
(
set -euo pipefail

# Custom runner images need not include Docker.
if ! command -v dockerd >/dev/null 2>&1; then
  exit 0
fi

interface=$(ip -j route show default | jq -er '.[0].dev')
mtu=$(cat "/sys/class/net/${interface}/mtu")
if ! [[ "${mtu}" =~ ^[0-9]+$ ]] || (( mtu < 1280 || mtu > 65535 )); then
  echo "Invalid Docker network MTU: ${mtu}" >&2
  exit 1
fi

# --mtu only configures the default bridge. Kind copies that bridge's option;
# Docker 27+ can also default all NEW user-defined bridges (including Compose).
major=$(dockerd --version | awk '{print $3}' | cut -d. -f1)
if ! [[ "${major}" =~ ^[0-9]+$ ]]; then
  echo "Cannot determine Docker Engine version" >&2
  exit 1
fi

install -d -m 755 /etc/docker
config=/etc/docker/daemon.json
tmp=$(mktemp /etc/docker/daemon.json.XXXXXX)
trap 'rm -f "${tmp}"' EXIT
existing=/dev/null
if [ -e "${config}" ]; then
  existing=${config}
fi
jq -s --argjson mtu "${mtu}" --argjson major "${major}" '
  (if length == 0 then {} elif length == 1 and (.[0] | type) == "object" then .[0]
   else error("Docker daemon config must be an object") end)
  | .mtu = $mtu
  | if $major >= 27 then
      .["default-network-opts"].bridge["com.docker.network.driver.mtu"] = ($mtu | tostring)
    else . end
' "${existing}" > "${tmp}"
dockerd --validate --config-file "${tmp}"
chmod 644 "${tmp}"
mv "${tmp}" "${config}"

echo "Configuring Docker networks for host ${interface} MTU ${mtu} (Engine ${major})"
systemctl restart docker.service
actual=$(docker network inspect bridge --format '{{ index .Options "com.docker.network.driver.mtu" }}')
if [ "${actual}" != "${mtu}" ]; then
  echo "Docker bridge MTU ${actual} does not match host MTU ${mtu}" >&2
  exit 1
fi
)

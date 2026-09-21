#!/bin/bash
# Manual regression on a DISPOSABLE runner VM with Cloud's Kind fixture deployed.
# Apply ../docker-network.sh BEFORE creating Kind, omit Cloud's network workaround,
# then deploy the fixture and wait for its executor. Requires openruntimes/node:v5-22
# in the executor's Docker daemon (the normal executor readiness gate pulls it).
# Run with Docker access and HOME/KUBECONFIG pointing at the kind-cloud context.
set -euo pipefail

interface=$(ip -j route show default | jq -er '.[0].dev')
mtu=$(cat "/sys/class/net/${interface}/mtu")
for network in bridge kind; do
  actual=$(docker network inspect "$network" --format '{{ index .Options "com.docker.network.driver.mtu" }}')
  test "$actual" = "$mtu"
  echo "$network MTU: $actual (matches $interface)"
done

k=(kubectl --context kind-cloud --namespace cloud)
executor=$("${k[@]}" get pod -l app=openruntimes-executor -o jsonpath='{.items[0].metadata.name}')
appwrite=$("${k[@]}" get pod -l app=appwrite -o jsonpath='{.items[0].metadata.name}')
ip=$("${k[@]}" get pod "$executor" -o jsonpath='{.status.podIP}')
actual=$("${k[@]}" exec "$executor" -c docker -- cat /sys/class/net/eth0/mtu)
test "$actual" = "$mtu"
echo "executor pod MTU: $actual"

# Keep the nested daemon's own MTU/subnet untouched, including its overlap with
# Kind's 172.18.0.0/16. Exercise large responses from a nested runtime to a pod,
# not just a host-side curl or a network-inspect assertion.
"${k[@]}" exec "$executor" -c docker -- docker run -d --name gcrunner-mtu-response \
  --network appwrite -p 18080:8080 --entrypoint node openruntimes/node:v5-22 \
  -e 'require("http").createServer((req,res)=>{res.writeHead(200,{"Content-Length":8388608});res.end(Buffer.alloc(8388608));}).listen(8080,"0.0.0.0")'
trap '"${k[@]}" exec "$executor" -c docker -- docker rm -f gcrunner-mtu-response >/dev/null' EXIT
expected=$(head -c 8388608 /dev/zero | sha256sum | cut -d' ' -f1)
"${k[@]}" exec "$executor" -c docker -- docker network inspect appwrite --format 'nested network: {{json .IPAM.Config}} {{json .Options}}'
"${k[@]}" exec "$executor" -c docker -- docker exec gcrunner-mtu-response cat /sys/class/net/eth0/mtu
# Expand the positional arguments inside the pod, not in the host shell.
# shellcheck disable=SC2016
"${k[@]}" exec "$appwrite" -c appwrite -- sh -c '
set -eu
trap "rm -f /tmp/gcrunner-mtu-payload" EXIT
for i in $(seq 1 10); do
  curl --fail --silent --show-error --retry 3 --retry-connrefused --retry-delay 1 \
    --max-time 30 --output /tmp/gcrunner-mtu-payload "$1"
  test "$(wc -c </tmp/gcrunner-mtu-payload)" = 8388608
  echo "$2  /tmp/gcrunner-mtu-payload" | sha256sum -c -
  echo "large-response $i: 8388608 bytes, SHA256 verified"
done
' sh "http://${ip}:18080/payload" "$expected"
echo LARGE_RESPONSE_PASSED

# Also traverse the actual GCE NIC. Compare with a host-side reference download,
# rather than coupling this network regression to a tool's release/checksum pin.
# An operator can substitute any stable HTTPS payload of at least 1 MiB.
url=${MTU_E2E_DOWNLOAD_URL:-https://proof.ovh.net/files/10Mb.dat}
reference=$(mktemp)
trap 'rm -f "$reference"; "${k[@]}" exec "$executor" -c docker -- docker rm -f gcrunner-mtu-response >/dev/null' EXIT
curl --fail --location --silent --show-error --max-time 60 --output "$reference" "$url"
expected=$(sha256sum "$reference" | cut -d' ' -f1)
bytes=$(wc -c < "$reference" | tr -d '[:space:]')
test "$bytes" -ge 1048576
"${k[@]}" exec "$executor" -c docker -- docker run --rm --network appwrite \
  --entrypoint node openruntimes/node:v5-22 -e '
(async () => {
  const [url, expectedHash, expectedLength] = process.argv.slice(1);
  const response = await fetch(url, {signal: AbortSignal.timeout(60000)});
  if (!response.ok) throw Error(response.status);
  const body = Buffer.from(await response.arrayBuffer());
  const hash = require("crypto").createHash("sha256").update(body).digest("hex");
  if (body.length !== Number(expectedLength) || hash !== expectedHash) throw Error("Download mismatch");
  console.log("Nested external HTTPS download:", body.length, "bytes, SHA256 verified against host reference");
})().catch(error => { console.error(error); process.exit(1); });
' "$url" "$expected" "$bytes"

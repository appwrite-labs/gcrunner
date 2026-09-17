#!/bin/bash -e
################################################################################
##  File:  preload-images.sh
##  Desc:  Pull the images every cloud CI job needs into the image's Docker
##         store so a job only transfers the layers that changed.
################################################################################
REGISTRY="europe-west3-docker.pkg.dev"
CLOUD_REPOSITORY="${REGISTRY}/appwrite-gha-runners/ci/cloud"
HUB_IMAGES=(
    kindest/node:v1.32.2
    nats:2.12-alpine
    docker.dragonflydb.io/dragonflydb/dragonfly
)

curl -sf -H 'Metadata-Flavor: Google' \
    'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token' \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])' \
    | docker login -u oauth2accesstoken --password-stdin "${REGISTRY}"

for image in "${HUB_IMAGES[@]}"; do
    docker pull --quiet "${image}"
done

latest=$(gcloud artifacts docker images list "${CLOUD_REPOSITORY}" --include-tags \
    --filter='tags~^appwrite-dev-' --sort-by=~createTime --limit=1 --format='value(tags)' | cut -d, -f1)
if [ -n "${latest}" ]; then
    docker pull --quiet "${CLOUD_REPOSITORY}:${latest}"
fi

docker logout "${REGISTRY}"
echo "Preloaded images:"
docker image ls --format '  {{.Repository}}:{{.Tag}} {{.Size}}'

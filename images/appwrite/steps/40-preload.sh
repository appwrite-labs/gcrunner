#!/bin/bash -e
# Pull what every job pulls, so a job transfers only the layers that changed since the build.
set -o pipefail
registry=$(jq -r '.preload.registry' "${APPWRITE_TOOLSET}")

curl -sf -H 'Metadata-Flavor: Google' \
    'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token' \
    | jq -r '.access_token' \
    | docker login -u oauth2accesstoken --password-stdin "${registry}"

for image in $(jq -r '.preload.images[]' "${APPWRITE_TOOLSET}"); do
    docker pull --quiet "${image}"
done

jq -c '.preload.latest_tag_of[]' "${APPWRITE_TOOLSET}" | while read -r entry; do
    repository=$(jq -r '.repository' <<<"${entry}")
    prefix=$(jq -r '.tag_prefix' <<<"${entry}")
    tag=$(gcloud artifacts docker images list "${repository}" --include-tags \
        --filter="tags~^${prefix}" --sort-by=~createTime --limit=1 --format='value(tags)' | cut -d, -f1)
    if [ -z "${tag}" ]; then
        echo "no ${prefix}* tag found in ${repository}" >&2
        exit 1
    fi
    docker pull --quiet "${repository}:${tag}"
done

docker logout "${registry}"
echo "Preloaded images:"
docker image ls --format '  {{.Repository}}:{{.Tag}} {{.Size}}'

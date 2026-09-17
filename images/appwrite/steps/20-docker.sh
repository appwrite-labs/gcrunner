#!/bin/bash -e
# Jobs drive Docker without sudo.
for user in $(jq -r '.docker.group_members[]' "${APPWRITE_TOOLSET}"); do
    usermod -aG docker "${user}"
done

#!/bin/bash -e
################################################################################
##  File:  apply.sh
##  Desc:  Run the Appwrite customisations in steps/ in order, each reading
##         its settings from toolset.json.
################################################################################
export APPWRITE_TOOLSET="$(dirname "$0")/toolset.json"
export DEBIAN_FRONTEND=noninteractive

for step in "$(dirname "$0")"/steps/*.sh; do
    echo "=== $(basename "${step}")"
    bash -e "${step}"
done

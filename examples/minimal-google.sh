#!/bin/bash
export LOCATION=""
export ZONE=""
export PROJECT_ID=""

docker run --pull always -it --rm \
    -v $(echo ~)/.config/gcloud:/root/.config/gcloud \
    -v "$(pwd)":"/etc/ei-agent/config/" \
    -e PROJECT_ID=$PROJECT_ID \
    -e LOCATION=$LOCATION \
    -e ZONE=$ZONE \
    -e CONFIG=/etc/ei-agent/config/minimal-google.yaml entigolabs/entigo-infralib-agent

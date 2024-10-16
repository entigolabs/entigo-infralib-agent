#!/bin/bash
export LOCATION="europe-north1"
export ZONE="europe-north1-a"
export PROJECT_ID="entigo-infralib-agent"

docker run --pull always -it --rm -v $(echo ~)/.config/gcloud:/root/.config/gcloud -v "$(pwd)":"/etc/ei-agent/conf" -e PROJECT_ID=$PROJECT_ID -e LOCATION=$LOCATION -e ZONE=$ZONE -e CONFIG=/etc/ei-agent/conf/minimal-google.yaml entigolabs/entigo-infralib-agent

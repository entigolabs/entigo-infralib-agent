#!/bin/bash
export LOCATION="europe-north1"
export ZONE="europe-north1-a"
export PROJECT_ID="entigo-infralib-agent"

docker run --pull always -it --rm -v $(echo ~)/.config/gcloud:/root/.config/gcloud -v "$(pwd)/minimal-google.yaml":"/etc/ei-agent/config.yaml" -e PROJECT_ID=$PROJECT_ID -e LOCATION=$LOCATION -e ZONE=$ZONE -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent

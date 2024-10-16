#!/bin/bash
export AWS_REGION="eu-north-1"
docker run --pull always -it --rm -v "$(pwd)":"/etc/ei-agent/conf" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/conf/minimal-aws.yaml entigolabs/entigo-infralib-agent

#!/bin/bash
# AWS-specific functions

export PROVIDER="aws"

# Get working directory for this environment
get_work_dir() {
    echo "/tmp/project"
}

# Copy project files from bucket to local directory
# Usage: copy_from_bucket <bucket> <source_path> <dest_path>
copy_from_bucket() {
    local bucket="$1"
    local source_path="$2"
    local dest_path="$3"
    local exclude_pattern=""

    if [ "$TERRAFORM_CACHE" != "true" ]; then
        exclude_pattern="--exclude *.terraform/*"
    fi

    aws s3 cp "s3://${bucket}/${source_path}" "${dest_path}" --recursive --no-progress --quiet ${exclude_pattern}
}

# Copy file to bucket
# Usage: copy_to_bucket <local_file> <bucket> <dest_path>
copy_to_bucket() {
    local local_file="$1"
    local bucket="$2"
    local dest_path="$3"

    aws s3 cp "${local_file}" "s3://${bucket}/${dest_path}" --no-progress --quiet
}

# Sync terraform cache to bucket
sync_terraform_cache() {
    local bucket="$1"
    local prefix="$2"

    echo "Syncing .terraform back to bucket"
    aws s3 sync .terraform "s3://${bucket}/steps/${prefix}/.terraform" --no-progress --quiet --delete
}

# Fetch plan artifact for apply stage
fetch_plan_artifact() {
    if [ ! -d /tmp/project/steps/$TF_VAR_prefix ]; then
        echo "Unable to find plan! /tmp/project/steps/$TF_VAR_prefix"
        exit 4
    fi
    cd "/tmp/project/steps/$TF_VAR_prefix"
}

# Upload plan artifact after plan stage
upload_plan_artifact() {
    cd ../..
    tar -czf tf.tar.gz "steps/$TF_VAR_prefix"
}

# Get Kubernetes credentials
get_k8s_credentials() {
    aws eks update-kubeconfig --name $KUBERNETES_CLUSTER_NAME --region $AWS_REGION
}

# Get ArgoCD hostname
get_argocd_hostname() {
    kubectl get ingress -n ${ARGOCD_NAMESPACE} -l app.kubernetes.io/component=server -o jsonpath='{.items[*].spec.rules[*].host}'
}

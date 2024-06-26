Manifests for testing Cloud Deploy. Steps:

```shell
gcloud deploy apply --file=target-plan.yaml --region=europe-north1
gcloud deploy apply --file=target-apply.yaml --region=europe-north1
gcloud deploy apply --file=delivery-pipeline.yaml --region=europe-north1
gcloud deploy releases create --delivery-pipeline=entigo-infralib --skaffold-file=skaffold.yaml --region=europe-north1 entigo-infralib-001
```
```shell
gcloud deploy releases promote --delivery-pipeline=entigo-infralib --release=entigo-infralib-001 --region=europe-north1 # promotes to the next stage in the pipeline, starts a rollout
gcloud deploy rollouts list --delivery-pipeline=entigo-infralib --release=entigo-infralib-001 --region=europe-north1
gcloud deploy rollouts approve --delivery-pipeline=entigo-infralib --release=entigo-infralib-001 --region=europe-north1 entigo-infralib-001-to-apply-cloudrun-job-0001
```

Additionally, creating a release renders your application using skaffold and saves the output as a point-in-time reference that's used for the duration of that release.
When a release is created, it's automatically rolled out to the first target in the pipeline (unless approval is required, which is covered in a later step of this tutorial).
Any target can require an approval before a release promotion can occur. This is designed to protect production and sensitive targets from accidentally promoting a release before it's been fully vetted and tested.
approvalState is NEEDS_APPROVAL and the state is PENDING_APPROVAL

https://github.com/GoogleCloudPlatform/cloud-deploy-tutorials/tree/main/tutorials/e2e-run

Debug

```
skaffold apply --filename=skaffold.yaml --cloud-run-project=entigo-infralib --cloud-run-location=europe-north1 --rpc-port=0 --event-log-file=skaffold-event-logs.txt --label="skaffold.dev/run-id=1b7wfqszmgykehuhtavdtc9s93jq5zuz2gpz8b1nfw3fw67c42o" entigo-infralib-plan.yaml
```
## Example client configurations

These are example configurations on the client side.

## Example usage 

Fill the exported values in either `minimal-aws.sh` or `minimal-google.sh`. Then replace the DNS `parent_zone_id` placeholder with the actual parent zone id in the corresponding `*.yaml` file. After that run the script.

## To remove all created resources
Run destroy pipelines in reverse order as they appear in the configuration.

For example:
1) (AWS only) Enable all the destroy pipeline transitions.
2) Execute in reverse order the destroy AWS CodePipelines or Google Cloud Run Jobs, by starting the plan-destroy job before the apply-destroy job.
3) (AWS only) Approve the planned changes in the AWS CodePipeline.
4) Run the agent delete command after the destroy pipelines have successfully finished, by adding `ei-agent delete` to the previously executed script. Add `--delete-bucket` flag if you want to delete the infralib bucket as well.

# entigo-infralib-agent

Entigo infralib agent prepares an AWS Account or Google Cloud Project for Entigo infralib terraform modules.
Creates the required resources for S3, DynamoDB, CloudWatch, CodeBuild, CodePipeline, and IAM roles and policies.
Executes CodePipelines or Cloud Deploy DeliveryPipelines which apply the specified Entigo infralib terraform modules.

* [Compiling Source](#compiling-source)
* [Requirements](#requirements)
* [Docker](#docker)
    * [Building a local Docker image](#building-a-local-docker-image)
    * [Running the Docker image](#running-the-docker-image)
* [Commands](#commands)
    * [Bootstrap](#bootstrap)
    * [Run](#run)
    * [Delete](#delete)
    * [Merge](#merge)
* [Config](#config)
  * [Overriding config values](#overriding-config-values)

## Compiling Source

```go build -o bin/ei-agent main.go```

## Requirements

AWS Service Account with administrator access, credentials provided by AWS or environment variables.

or

Google Cloud Service Account with owner access, credentials provided by GCP or gcloud cli tool.

## Docker

Prebuilt Docker image is available from

[Docker Hub](https://hub.docker.com/r/entigolabs/entigo-infralib-agent) `entigolabs/entigo-infralib-agent`

or

[Amazon ECR Gallery](https://gallery.ecr.aws/entigolabs/entigo-infralib-agent) `public.ecr.aws/entigolabs/entigo-infralib-agent`

### Building a local Docker image

```docker build -t entigolabs/entigo-infralib-agent .```

### Running the Docker image

By default, the docker image executes the [Run](#run) command. Config.yaml needs to be mounted into the container. This is required only for the first run or when overriding an existing config.

```docker run --pull always -it --rm -v "$(pwd)/config.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent```

To execute the [bootstrap](#bootstrap), override the default command.

```docker run --pull always -it --rm -v "$(pwd)/config.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent ei-agent bootstrap```

## Commands

For bootstrap and run commands you must either provide a config file or an aws-prefix value. This is required for creating and finding AWS resources. Bootstrap adds that value as an environment variable for the agent pipeline.

### bootstrap

Creates the required AWS resources and a codepipeline for executing the agent. If the pipeline already exists, the agent image version will be updated if needed and a new execution will be started.

OPTIONS:
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* aws-prefix - prefix used when creating cloud resources (default: **config prefix**) [$AWS_PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]

Example
```bash
bin/ei-agent bootstrap --config=config.yaml --aws-prefix=entigo-infralib
```

### run

Processes config steps, creates and executes CodePipelines which apply Entigo Infralib terraform modules.

OPTIONS:
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* aws-prefix - prefix used when creating cloud resources (default: **config prefix**) [$AWS_PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* allow-parallel - allow running steps in parallel on first execution cycle (default: **true**) [$ALLOW_PARALLEL]

Example
```bash
bin/ei-agent run --config=config.yaml --aws-prefix=entigo-infralib
```

### delete

Processes config steps, removes resources used by the agent, including buckets, pipelines, and roles/service accounts.
**Warning!** Execute destroy pipelines in reverse config order before running this command. This command will remove all pipelines and resources created by terraform will otherwise remain.

OPTIONS:
* config - config file path and name, only needed when overriding an existing config [$CONFIG]
* aws-prefix - prefix used when creating cloud resources (default: **config prefix**) [$AWS_PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* yes - skip confirmation prompt (default: **false**) [$YES]
* delete-bucket - delete the bucket used by terraform state (default: **false**) [$DELETE_BUCKET]

Example
```bash
bin/ei-agent delete --config=config.yaml --aws-prefix=entigo-infralib
```

### merge

Merges given base and patch config files and validates the result. Merged config is printed to stdout.

OPTIONS:
* base-config - base config file path and name [$BASE_CONFIG]
* config - patch config file path and name [$CONFIG]

Example
```bash
bin/ei-agent merge --base-config=base.yaml --config=patch.yaml
```

## Config

Config is provided with a yaml file:

```yaml
base_config:
  version: stable | semver
  profile: string
prefix: string
source: https://github.com/entigolabs/entigo-infralib-release
version: stable | semver
agent_version: latest | semver
base_image_version: stable | semver
steps:
  - name: string
    type: terraform | argocd-apps | terraform-custom 
    workspace: string
    before: string
    approve: minor | major | never | always
    version: stable | semver
    base_image_version: stable | semver
    remove: bool
    vpc_id: string
    vpc_subnet_ids: multiline string
    vpc_security_group_ids: multiline string
    kubernetes_cluster_name: string
    repo_url: string
    modules:
      - name: string
        source: string
        version: stable | semver
        http_username: string
        http_password: string
        remove: bool
        inputs: map[string]string
    provider:
      inputs: map[string]string
      aws:
        ignore_tags:
          key_prefixes: []string
          keys: []string
        default_tags:
          tags: map[string]string
      kubernetes:
        ignore_annotations: []string
        ignore_labels: []string
```
Complex values need to be as multiline strings with | symbol.

Config version is overwritten by step version which in turn is overwritten by module version. Default version is **stable**.
During merging, step name and workspace are used for identifying parent steps, modules are identified by name.

* base_config - base config, pulled from source
  * version - highest version of Entigo Infralib base config
  * profile - name of the config file without a suffix, empty string means no base config is used
* prefix - prefix used for AWS/GCloud resources, CodeCommit folders/files and terraform resources
* source - source repository for Entigo Infralib terraform modules
* version - version of Entigo Infralib terraform modules to use
* agent_version - image version of Entigo Infralib Agent to use
* base_image_version - image version of Entigo Infralib Base Image to use, default uses the version from step
* steps - list of steps to execute
  * name - name of the step
  * type - type of the step
  * workspace - terraform workspace to use
  * before - for patch config, name of the step in the same workspace that this step should be executed before
  * approve - approval type for the step, only applies when terraform needs to change resources, based on semver. Destroying resources always requires manual approval. Approve always means that manual approval is required, never means that agent approves automatically. Custom terraform steps only support values `always` and `never`, default **always**
  * version - version of Entigo Infralib terraform modules to use
  * base_image_version - image version of Entigo Infralib Base Image to use, default uses the newest module version
  * remove - whether to remove the step during merge or not, default **false**
  * vpc_id - vpc id for code build
  * vpc_subnet_ids - vpc subnet ids for code build
  * vpc_security_group_ids - vpc security group ids for code build
  * kubernetes_cluster_name - kubernetes cluster name for argocd-apps steps
  * argocd_namespace - kubernetes namespace for argocd-apps steps, default **argocd**
  * repo_url - for argocd-apps steps, repo to use for cloning
  * modules - list of modules to apply
    * name - name of the module
    * source - source of the terraform module, can be an external git repository beginning with git:: or git@
    * version - version of the module to use
    * http_username - username for external repository authentication
    * http_password - password for external repository authentication
    * remove - whether to remove the module during merge or not, default **false**
    * inputs - map of inputs for the module, string values need to be quoted
  * provider - provider values to add
    * inputs - variables for provider tf file
    * aws - aws provider default and ignore tags to add
    * kubernetes - kubernetes provider ignore annotations and labels to add

### Overriding config values

Step, module and input field values can be overwritten by using replacement tags `{{ }}`.

Replacement tags can be overwritten by values that are stored in the AWS SSM Parameter Store `ssm` and Google Cloud Secret Manager `gcsm`, config itself or custom agent logic. It's also possible to use the keyword `output` instead to let agent choose the correct service for getting the value.

For example, `{{ .ssm.stepName.moduleName.key-1/key-2 }}` will be overwritten by the value of the SSM Parameter Store parameter `/entigo-infralib/config.prefix-stepName-moduleName-parentStep.workspace/key-1/key-2`.
If the parameter type is StringList then it's possible to use an index to get a specific value, e.g `{{ .ssm.stepName.moduleName.key-1/key-2[0] }}` or a slice by using a range, e.g [0-1].

Custom SSM parameter example `{{ .ssm-custom.key }}` will be overwritten by the value of the custom SSM parameter `key`.
For custom GCloud SM, replace the ssm with gcsm.

Config example `{{ .config.prefix }}` will be overwritten by the value of the config field `prefix`. Config replacement does not support indexed paths.

Agent example `{{ .agent.version.step.module }}` will be overwritten by the value of the specified module version that's currently being applied or a set version, e.g `v0.8.4`. Agent replacement also supports account id using key accountId.
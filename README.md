# Entigo Infralib Agent

Entigo infralib agent prepares an AWS Account or Google Cloud Project for Entigo infralib terraform modules.
Creates the required resources for S3/storage, DynamoDB, CloudWatch, CodeBuild/Cloud Run Jobs, CodePipeline/Delivery Pipeline, and IAM roles and policies.
Executes pipelines which apply the specified Entigo infralib terraform modules. During subsequent runs, the agent will update the modules to the latest version and apply any config changes.

* [Compiling Source](#compiling-source)
* [Requirements](#requirements)
* [Docker](#docker)
    * [Building a local Docker image](#building-a-local-docker-image)
    * [Running the Docker image](#running-the-docker-image)
* [Commands](#commands)
    * [Bootstrap](#bootstrap)
    * [Run](#run)
    * [Update](#update)
    * [Delete](#delete)
    * [Service Account](#service-account)
* [Config](#config)
  * [Overriding config values](#overriding-config-values)
  * [Including terraform files in steps](#including-terraform-files-in-steps)

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

For bootstrap, run and update commands you must either provide a config file or a prefix value. This is required for creating and finding AWS resources. Bootstrap adds that value as an environment variable for the agent pipeline.

### bootstrap

Creates the required AWS resources and a codepipeline for executing the agent. If the pipeline already exists, the agent image version will be updated if needed and a new execution of the run command will be started.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$AWS_PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]

Example
```bash
bin/ei-agent bootstrap --config=config.yaml --prefix=infralib
```

### run

Processes config steps, creates and executes CodePipelines which apply Entigo Infralib terraform modules.
Run command only executes a single cycle of the pipeline. Can be used to apply config changes.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$AWS_PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* allow-parallel - allow running steps in parallel on first execution cycle (default: **true**) [$ALLOW_PARALLEL]

Example
```bash
bin/ei-agent run --config=config.yaml --prefix=infralib
```

### update

Processes config steps, creates and executes CodePipelines which apply Entigo Infralib terraform modules.
Update command updates all modules to the latest or specified versions. Returns if there are no updates available.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$AWS_PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]

Example
```bash
bin/ei-agent update --config=config.yaml --prefix=infralib
```

### delete

Processes config steps, removes resources used by the agent, including buckets, pipelines, and roles/service accounts.
**Warning!** Execute destroy pipelines in reverse config order before running this command. This command will remove all pipelines and resources created by terraform will otherwise remain.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$AWS_PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* yes - skip confirmation prompt (default: **false**) [$YES]
* delete-bucket - delete the bucket used by terraform state (default: **false**) [$DELETE_BUCKET]

Example
```bash
bin/ei-agent delete --config=config.yaml --prefix=infralib
```

### service-account

Creates a service account and a key for the account. The key is stored in the AWS SSM Parameter Store or Google Cloud Secret Manager.
This account can be used for running the agent in a CI/CD pipeline.

OPTIONS:
* prefix - prefix used when creating cloud resources [$AWS_PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]

Example
```bash
bin/ei-agent service-account --prefix=infralib
```

## Config

Config is provided with a yaml file:

```yaml
prefix: string
sources:
  - url: https://github.com/entigolabs/entigo-infralib-release
    version: stable | semver
    include: []string
    exclude: []string
agent_version: latest | semver
base_image_source: string
base_image_version: stable | semver
steps:
  - name: string
    type: terraform | argocd-apps
    approve: minor | major | never | always
    base_image_source: string
    base_image_version: stable | semver
    vpc:
      attach: bool
      id: string
      subnet_ids: multiline string
      security_group_ids: multiline string
    kubernetes_cluster_name: string
    repo_url: string
    modules:
      - name: string
        source: string
        version: stable | semver
        http_username: string
        http_password: string
        inputs: map[string]interface{}
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

Source version is overwritten by module version. Default version is **stable** which means latest release of the source repository.

* prefix - prefix used for AWS/GCloud resources, CodeCommit folders/files and terraform resources, limit 10 characters
* sources - list of source repositories for Entigo Infralib modules
  * url - url of the source repository
  * version - highest version of Entigo Infralib modules to use
  * include - list of module sources to exclusively include from the source repository
  * exclude - list of module sources to exclude from the source repository
* agent_version - image version of Entigo Infralib Agent to use
* base_image_source - source of Entigo Infralib Base Image to use
* base_image_version - image version of Entigo Infralib Base Image to use, default uses the version from step
* steps - list of steps to execute
  * name - name of the step
  * type - type of the step
  * approve - approval type for the step, only applies when terraform needs to change resources, based on semver. Destroying resources always requires manual approval. Approve always means that manual approval is required, never means that agent approves automatically, default **always**
  * base_image_source - source of Entigo Infralib Base Image to use
  * base_image_version - image version of Entigo Infralib Base Image to use, default uses the newest module version
  * vpc - vpc values to add
    * attach - attach vpc to code build/cloud run job, if other fields are empty then uses default vpc based on typed output of a vpc module, default **nil**. When nil, the value will be set based on the step type
    * id - vpc id for code build/cloud run job
    * subnet_ids - vpc subnet ids for code build/cloud run job
    * security_group_ids - vpc security group ids for code build/cloud run job
  * kubernetes_cluster_name - kubernetes cluster name for argocd-apps steps
  * argocd_namespace - kubernetes namespace for argocd-apps steps, default **argocd**
  * repo_url - for argocd-apps steps, repo to use for cloning
  * modules - list of modules to apply
    * name - name of the module
    * source - source of the terraform module, can be an external git repository beginning with git:: or git@
    * version - highest version of the module to use
    * http_username - username for external repository authentication
    * http_password - password for external repository authentication
    * inputs - **optional**, map of inputs for the module, string values need to be quoted. If missing, inputs are optionally read from a yaml file that must be located in the `./config/<stepName>` directory with a name `<moduleName>.yaml`.
  * provider - provider values to add
    * inputs - variables for provider tf file
    * aws - aws provider default and ignore tags to add
    * kubernetes - kubernetes provider ignore annotations and labels to add

### Overriding config values

Step, module and input field values can be overwritten by using replacement tags `{{ }}`.

Replacement tags can be overwritten by values that are stored in the AWS SSM Parameter Store `ssm` and Google Cloud Secret Manager `gcsm`, config itself or custom agent logic. It's also possible to use the keyword `output` instead to let agent choose the correct service for getting the value. There's also a special type based keyword `toutput` that uses an output from the specified type of step.

For example, `{{ .ssm.stepName.moduleName.key-1/key-2 }}` will be overwritten by the value of the SSM Parameter Store parameter `/entigo-infralib/config.prefix-stepName-moduleName-parentStep/key-1/key-2`.
If the parameter type is StringList then it's possible to use an index to get a specific value, e.g `{{ .ssm.stepName.moduleName.key-1/key-2[0] }}` or a slice by using a range, e.g [0-1].

It's possible to build a custom array by using yaml multiline string, even mixing replaced values with inputted values. For example creating a list of strings for terraform:
```yaml
inputs:
  key-1: |
    ["{{ .ssm.stepName.moduleName.key-1 }}", "value-1", "value-2"]
```

Custom SSM parameter example `{{ .ssm-custom.key }}` will be overwritten by the value of the custom SSM parameter `key`.
For custom GCloud SM, replace the ssm with gcsm.

Config example `{{ .config.prefix }}` will be overwritten by the value of the config field `prefix`. Config replacement does not support indexed paths.

Agent example `{{ .agent.version.step.module }}` will be overwritten by the value of the specified module version that's currently being applied or a set version, e.g `v0.8.4`. Agent replacement also supports account id using key accountId.

Infralib modules may use `{{ .tmodule.type }}` in their default input files to replace it with the name of the module used in the config.

### Including terraform files in steps

It's possible to include terraform files in steps by adding the files into a `./config/<stepName>/include` subdirectory. File names can't include `main.tf`, `provider.tf` or `backend.conf` as they are reserved for the agent. Files will be copied into the step directory which is used by terraform as step context.
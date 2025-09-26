# Entigo Infralib Agent

Entigo infralib agent prepares an AWS Account or Google Cloud Project for [Entigo Infralib modules](https://github.com/entigolabs/entigo-infralib).
Creates the required resources for S3/storage, DynamoDB, CloudWatch, CodeBuild/Cloud Run Jobs, CodePipeline/Delivery Pipeline, and IAM roles and policies.
Executes pipelines which apply the configured modules. During subsequent runs, the agent will update the modules to the latest version and apply any config changes.

* [Requirements](#requirements)
* [Compiling Source](#compiling-source)
* [Installation](#installation)
* [Docker](#docker)
    * [Building a local Docker image](#building-a-local-docker-image)
    * [Running the Docker image](#running-the-docker-image)
    * [Using with AWS Profile](#using-with-aws-profile)
* [Commands](#commands)
    * [Bootstrap](#bootstrap)
    * [Run](#run)
    * [Update](#update)
    * [Destroy](#destroy)
    * [Delete](#delete)
    * [Service Account](#service-account)
    * [Pull](#pull)
    * [Custom Parameters](#custom-parameters)
* [Config](#config)
  * [Including and excluding modules in sources](#including-and-excluding-modules-in-sources)
  * [Auto approval logic](#auto-approval-logic)
  * [Overriding config values](#overriding-config-values)
    * [List indexes](#list-indexes)
    * [Escaping replacement tags](#escaping-replacement-tags)
    * [Optional replacement tags](#optional-replacement-tags)
  * [Including files in steps](#including-files-in-steps)
  * [Including CA certificates](#including-ca-certificates)
  * [Notification API requests](#notification-api-requests)
  * [Encryption](#encryption)
* [Migration Helper](#migration-helper)
  * [Commands](#migration-commands)
      * [Migrate Config](#migrate-config)
      * [Migrate Plan](#migrate-plan)
      * [Migrate Validate](#migrate-validate)
  * [Import File](#import-file)
  * [Type Identifications File](#type-identifications-file)

## Requirements

AWS Service Account with administrator access, credentials provided by AWS or environment variables.

or

Google Cloud Service Account with owner access, credentials provided by GCP or gcloud cli tool.

## Compiling Source

```go build -o bin/ei-agent main.go```

## Installation

```go install github.com/entigolabs/entigo-infralib-agent@latest```

This will build and install the binary to $(go env GOPATH)/bin directory. Make sure that the directory is in your PATH.
When using this method, replace `ei-agent` with `entigo-infralib-agent` in the example commands.

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

### Using with AWS Profile

Example shell script that checks if the AWS SSO login is valid and runs the agent with the specified profile. Make sure to replace the value of `AWS_PROFILE` with your chosen AWS profile name. Initialize the AWS profile with `aws configure sso --profile profile-name` if it doesn't exist. Alternatively to SSO, you can manually edit the `~/.aws/credentials` file to add the profile with access key and secret key with token if using temporary credentials. In that case, you can remove the SSO login check from the script.

```shell
#!/bin/bash

AWS_REGION="eu-north-1"
AWS_PROFILE="profile-name"

echo "Checking SSO login..."
aws sts get-caller-identity --profile "${AWS_PROFILE}" || {
    echo "SSO login required. Running: aws sso login --profile ${AWS_PROFILE}"
    aws sso login --profile "${AWS_PROFILE}"
}

docker run --pull always -it --rm \
    -v "$(pwd)/config":"/etc/ei-agent/config" \
    -v "$(pwd)/config.yaml":"/etc/ei-agent/config.yaml" \
    -v "$HOME/.aws:/root/.aws:ro" \
    -e AWS_REGION="${AWS_REGION}" \
    -e AWS_PROFILE="${AWS_PROFILE}" \
    -e CONFIG=/etc/ei-agent/config.yaml \
    entigolabs/entigo-infralib-agent ei-agent run
```

## Commands

For bootstrap, run and update commands you must either provide a config file or a prefix value. This is required for creating and finding AWS resources. Bootstrap adds that value as an environment variable for the agent pipeline.

### bootstrap

Creates the required cloud resources and pipelines for executing the agent run and update commands. If the pipeline already exists, the agent image version will be updated if needed and a new execution of the run command will be started. For AWS, CodePipeline is used, for GCloud, Cloud Run Jobs are used.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* start - start pipeline execution after creating (default: **true**) [$START]

Example
```bash
bin/ei-agent bootstrap --config=config.yaml --prefix=infralib
```

### run

Processes config steps, creates and executes CodePipelines which apply Entigo Infralib terraform modules.
Run command only executes a single cycle of the pipeline. Can be used to apply config changes.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* skip-bucket-creation-delay - skip bucket creation delay (default: **false**) [$SKIP_BUCKET_CREATION_DELAY]
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - **optional** role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* steps - **optional** comma separated list of steps to run [$STEPS]
* allow-parallel - allow running steps in parallel on first execution cycle (default: **true**) [$ALLOW_PARALLEL]
* pipeline-type - pipeline execution type (local | cloud), local is meant to be run inside the infralib image (default: **cloud**) [$PIPELINE_TYPE]
* print-logs - print terraform/helm logs to stdout when using local execution (default: **true**) [$PRINT_LOGS]
* logs-path - **optional** path for storing terraform/helm logs when running local pipelines [$LOGS_PATH]
* terraform-cache - use terraform caching (default: **true**, when using pipeline-type local, default is **false**) [$TERRAFORM_CACHE]

Example
```bash
bin/ei-agent run --config=config.yaml --prefix=infralib
```

### update

Processes config steps, creates and executes CodePipelines which apply Entigo Infralib terraform modules.
Update command updates all modules to the latest or specified versions. Returns if there are no updates available.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* skip-bucket-creation-delay - skip bucket creation delay (default: **false**) [$SKIP_BUCKET_CREATION_DELAY]
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - **optional** role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* steps - **optional** comma separated list of steps to run [$STEPS]
* pipeline-type - pipeline execution type (local | cloud), local is meant to be run inside the infralib image (default: **cloud**) [$PIPELINE_TYPE]
* print-logs - print terraform/helm logs to stdout when using local execution (default: **true**) [$PRINT_LOGS]
* logs-path - **optional** path for storing terraform/helm logs when running local pipelines [$LOGS_PATH]
* terraform-cache - use terraform caching (default: **true**, when using pipeline-type local, default is **false**) [$TERRAFORM_CACHE]

Example
```bash
bin/ei-agent update --config=config.yaml --prefix=infralib
```

### destroy

Executes the destroy pipelines in reverse config order.
**Warning!** This will remove the resources provisioned by the step pipelines.
Agent automatically approves the changes.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* yes - skip confirmation prompt (default: **false**) [$YES]
* steps - **optional** comma separated list of steps to destroy [$STEPS]
* pipeline-type - pipeline execution type (local | cloud), local is meant to be run inside the infralib image (default: **cloud**) [$PIPELINE_TYPE]
* print-logs - print terraform/helm logs to stdout when using local execution (default: **true**) [$PRINT_LOGS]
* logs-path - **optional** path for storing terraform/helm  logs when running local pipelines [$LOGS_PATH]

Example
```bash
bin/ei-agent destroy --config=config.yaml --prefix=infralib
```

### delete

Processes config steps, removes resources used by the agent, including buckets, pipelines, and roles/service accounts.
**Warning!** Execute destroy pipelines in reverse config order before running this command. This command will remove all pipelines and resources created by terraform will otherwise remain.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* yes - skip confirmation prompt (default: **false**) [$YES]
* delete-bucket - delete the bucket used by terraform state (default: **false**) [$DELETE_BUCKET]
* delete-service-account - delete the service account created by service-account command (default: **false**) [$DELETE_SERVICE_ACCOUNT]

Example
```bash
bin/ei-agent delete --config=config.yaml --prefix=infralib
```

### service-account

Creates a service account and a key for the account. The key is stored in the AWS SSM Parameter Store or Google Cloud Secret Manager.
This account can be used for running the agent in a CI/CD pipeline.

Optionally, prefix will be used to pull the config and config is used to check for an encryption module. If present, the created key will be encrypted with customer encryption. More info in [Encryption](#encryption).

OPTIONS:
* config - config file path and name, only needed when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* terraform-cache - use terraform caching, adds the param to the bootstrapped agent (default: **true**) [$TERRAFORM_CACHE]

Example
```bash
bin/ei-agent service-account --prefix=infralib
```

### pull

Pulls agent config yaml and the config folders from the S3/GCloud bucket. Use the `force` flag to overwrite existing local files.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* force - overwrite existing local files, default **false**. **Warning!** Force deletes the `/config` subfolder before writing. [$FORCE]

Example
```bash
bin/ei-agent pull --prefix=infralib
```

### Custom Parameters

Agent has helpful commands for managing custom parameters that can be used in the config file with the `{{ .output-custom.key }}` replacement tag. These commands are:
* `add-custom` - for adding or updating a custom parameter
* `delete-custom` - for deleting a custom parameter
* `get-custom` - for getting a custom parameter value
* `list-custom` - for listing all custom parameters created by the agent

Optionally, prefix will be used to pull the config and config is used to check for an encryption module. If present, the created key will be encrypted with customer encryption. More info in [Encryption](#encryption).

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* config - config file path and name, only needed when overriding an existing config [$CONFIG]
* prefix - prefix used when creating cloud resources [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* key - key for the custom parameter [$KEY]
* value - value for the custom parameter [$VALUE]
* overwrite - overwrite existing custom parameter value, default **false** [$OVERWRITE]

Example
```bash
bin/ei-agent add-custom --key=custom-key --value=custom-value
```

## Config

Config is provided with a yaml file:

```yaml
prefix: string
sources:
  - url: https://github.com/entigolabs/entigo-infralib-release | path
    version: stable | semver | branch
    include: []string
    exclude: []string
    force_version: bool
    username: string
    password: string
    repo_path: string
    ca_file: string
destinations:
  - name: string
    git:
      url: string
      key: string
      key_password: string
      username: string
      password: string
      author_name: string
      author_email: string
      insecure: bool
      ca_file: string
notifications:
  - name: string
    context: string
    message_types: []string
    slack:
      token: string
      channel_id: string
    teams:
      webhook_url: string
    api:
      url: string
      key: string
agent_version: latest | semver
base_image_source: string
base_image_version: stable | semver
steps:
  - name: string
    type: terraform | argocd-apps
    approve: minor | major | never | always | force | reject
    manual_approve_run: always | changes | removes | never | reject
    manual_approve_update: always | changes | removes | never | reject
    base_image_source: string
    base_image_version: stable | semver
    vpc:
      attach: bool
      id: string
      subnet_ids: multiline string
      security_group_ids: multiline string
    kubernetes_cluster_name: string
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

* prefix - prefix used for AWS/GCloud resources, bucket folders/files and terraform resources, limit 10 characters, overwritten by the prefix flag/env var
* sources - list of source repositories for Entigo Infralib modules
  * url - url of the source repository or path to the local directory. Path must start with `./` or `../` Path will set force_version to true and use `local` as the version. Path only works with the local pipeline execution type.
  * version - highest version of Entigo Infralib modules to use
  * include - list of module sources to exclusively include from the source repository
  * exclude - list of module sources to exclude from the source repository
  * force_version - sets the specified version to all modules that use this source, useful for specifying a branch or tag instead of semver, default **false**. **Warning!** Before changing from true to false, force a version that follows semver.
  * username - username for git authentication
  * password - password for git authentication, it's recommended to use custom replacement tags, e.g. `"{{ .output-custom.git-password}}"`
  * repo_path - path to the git repository root directory, default uses Go's TempDir to create a directory named after the repository url. Use debug logging to see the path. **Warning!** Agent prunes the repo to match the remote.
  * ca_file - name of the CA certificate file in the `./ca-certificates` folder to use for git authentication
* destinations - list of destinations where the agent will push the generated step files, in addition to the default bucket
  * name - name of the destination
  * git - git repository must be accessible by the agent. For authentication, use either key or username/password. For the key and password, it's recommended to use custom replacement tags, e.g. `"{{ .output-custom.git-key }}"`
    * url - url of the git repository
    * key - PEM encoded private key for authentication
    * key_password - optional, password for the private key
    * insecure_host_key - accept any host key when using private key, default **false**
    * username - username for authentication
    * password - password for authentication
    * author_name - author name for commits, default **Entigo Infralib Agent**
    * author_email - author email for commits, default **no-reply@localhost**
    * insecure - allow insecure connection, default **false**
    * ca_file - name of the CA certificate file in the `./ca-certificates` folder to use for git authentication
* notifications - send notifications with selected types, each notifier can only use one subtype
  * name - name of the notifier
  * context - optional, extra context added to the notification
  * message_types - list of types of messages to send, possible values `started | approvals | progress | success | failure`, default **`[approvals, failure]`**
  * api - send notifications to a custom API
    * url - url for the api
    * key - unique identifier for the api
  * slack - send notifications to slack
    * token - slack access token, it's recommended to use custom replacement tags, e.g. `"{{ .output-custom.slack-token }}"`
    * channel_id - slack channel id
  * teams - send notifications to teams
    * webhook_url - webhook url for the teams channel, possible options include Teams Workflow or Power Automate, more info in [go-teams-notify GitHub](https://github.com/atc0005/go-teams-notify?tab=readme-ov-file#using-teams-client-workflows-context-option)
* agent_version - image version of Entigo Infralib Agent to use
* base_image_source - source of Entigo Infralib Base Image to use
* base_image_version - image version of Entigo Infralib Base Image to use, default uses the version from step
* steps - list of steps to execute
  * name - name of the step
  * type - type of the step
  * approve - **deprecated**, approval type for the step, possible values `minor | major | never | always | force | reject`, default **always**. More info in [Auto approval logic](#auto-approval-logic)
  * manual_approve_update - approval type for the step when using the update command, possible values `always | changes | removes | never | reject`, default **removes**. More info in [Auto approval logic](#auto-approval-logic)
  * manual_approve_run - approval type for the step when using the run command, possible values `always | changes | removes | never | reject`, default **changes**. More info in [Auto approval logic](#auto-approval-logic)
  * base_image_source - source of Entigo Infralib Base Image to use
  * base_image_version - image version of Entigo Infralib Base Image to use, default uses the newest module version
  * vpc - vpc values to add
    * attach - attach vpc to code build/cloud run job, if other fields are empty then uses default vpc based on typed output of a vpc module, default **nil**. When nil, the value will be set based on the step type, for `argocd-apps` steps the value will be set to `true`
    * id - vpc id for code build/cloud run job, gcloud default `{{ .toutput.vpc.vpc_name }}`, aws default `{{ .toutput.vpc.vpc_id }}`
    * subnet_ids - vpc subnet ids for code build/cloud run job, gcloud default `[{{ .toutput.vpc.private_subnets[0] }}]`, aws default `[{{ .toptout.vpc.control_subnets | .toutput.vpc.private_subnets }}]`
    * security_group_ids - vpc security group ids for code build/cloud run job, gcloud no default, aws default `[{{ .toutput.vpc.pipeline_security_group }}]`
  * kubernetes_cluster_name - kubernetes cluster name for argocd-apps steps, gcloud default `{{ .toutput.gke.cluster_name }}`, aws default `{{ .toutput.eks.cluster_name }}`
  * argocd_namespace - kubernetes namespace for argocd-apps steps, default **argocd**
  * modules - list of modules to apply
    * name - name of the module
    * source - source of the terraform module, can be an external git repository beginning with git:: or git@
    * version - highest version of the module to use
    * http_username - username for external repository authentication
    * http_password - password for external repository authentication
    * inputs - **optional**, map of inputs for the module, string values need to be quoted. If missing, inputs are optionally read from a yaml file that must be located in the `./config/<stepName>` directory with a name `<moduleName>.yaml`
  * provider - provider values to add
    * inputs - variables for provider tf file
    * aws - aws provider default and ignore tags to add
    * kubernetes - kubernetes provider ignore annotations and labels to add

### Including and excluding modules in sources

Agent associates modules with sources for requesting module files.
Module association is done by checking if a source includes a module.
This is done by checking the source list from top to bottom.
Source includes a module if:
* source config `include` property contains the module source
* source config `exclude` property does not contain the module source
* module source path is a subdirectory in source `modules` folder

For modules in steps with type `argocd-apps`, agent currently prefixes the `k8s` part of the path in the module source automatically.

Example
```yaml
sources:
  - url: https://github.com/entigolabs/entigo-infralib-release
    exclude: ["argocd"]
    include:
      - aws/eks
  - url: https://github.com/entigolabs/entigo-infralib
```

### Auto approval logic

Each step can be configured how it automatically approves infrastructure changes for the agent's `run` and `update` commands. To decide when to auto approve changes. If the planning stage of a step finds no changes, then the pipeline apply stage will be skipped. If only one of the `manual_approve_*` properties is set for a step, then the other property uses the default value. Possible values for the `manual_approve_run` and `manual_approve_update` are:
* `always` - will ask for user manual approval when resources are added, changed or removed.
* `changes` - will ask for user manual approval when resources are changed or removed. (default for `manual_approve_run`).
* `removes` - will ask for user manual approval when resources are removed (default for `manual_approve_update`).
* `never` - will never ask for manual approval. (**DANGEROUS!**)
* `reject` - stop the pipeline instead of approving, marks the step as failed. This can be used to generate plan files without applying them.

Step property `approve` has been deprecated and replaced by `manual_approve_run` and `manual_approve_update`. If none of those fields is set then `approve` will be used with default `always` value for backwards compatibility.

When using the `approve` property, auto approve type is only considered when resources will be changed. Adding resources doesn't require manual approval. Destroying resources always requires manual approval, except when using type `force`. Approve `always` means that manual approval is required, `never` means that agent approves automatically. Types `major` and `minor` require manual approval only when any of the step modules has a major or minor semver version change. Modules with external source require manual approval.

### Overriding config values

Step, module and input field values can be overwritten by using replacement tags `{{ .type.key }}`. Possible replacement tags are:

| Type            | Key / Format                | Example                         | Description                                                                               |
|-----------------|-----------------------------|---------------------------------|-------------------------------------------------------------------------------------------|
| `agent`         | version.stepName.moduleName | `.agent.version.infra.eks`      | Configured version of the specified module.                                               |
|                 | accountId                   | `.agent.accountId`              | Configured AWS account ID.                                                                |
|                 | region                      | `.agent.region`                 | Configured cloud provider region.                                                         |
| `config`        | fieldName                   | `.config.prefix`                | Value from the provided config field. Config replacement does not support indexed paths.  |
| `module`        | name                        | `.module.name`                  | Name of the module itself (for module inputs and input files only).                       |
|                 | source                      | `.module.source`                | Source of the module itself (for module inputs and input files only).                     |
| `optout`        | stepName.moduleName.key     | `.optout.infra.eks.cluster_arn` | Optional value from Terraform output from specific step/module. Defaults to empty string. |
| `output`        | stepName.moduleName.key     | `.output.infra.eks.cluster_arn` | Value from Terraform output from specific step/module.                                    |
| `output-custom` | key                         | `.output-custom.param-key`      | Value from AWS SSM parameter or GCloud SM.                                                |
| `step`          | name                        | `.step.name`                    | Name of the step containing the module.                                                   |
| `tmodule`       | type                        | `.tmodule.eks`                  | Name of the module with a specified type.                                                 |
| `toptmodule`    | type                        | `.tmodule.eks`                  | Optional name of the module with a specified type.                                        |
| `toptout`       | type.key                    | `.toptout.eks.cluster_arn`      | Optional value from Terraform output based on module type. Defaults to empty string.      |
| `toutput`       | type.key                    | `.toutput.eks.cluster_arn`      | Value from Terraform output based on module type.                                         |
| `tsmodule`      | type                        | `.tsmodule.eks`                 | Name of the typed module in the current step.                                             |

For output types, if the value is not found from terraform output, then the value is requested from AWS SSM Parameter Store or Google Cloud Secret Manager.

For example, `{{ .output.stepName.moduleName.key-1 }}` will be overwritten with the value from terraform output `moduleName__key-1`. As a fallback, uses SSM Parameter Store parameter `/entigo-infralib/config.prefix-stepName-moduleName-parentStep/key-1`.

It's possible to build a custom array by using yaml multiline string, even mixing replaced values with inputted values. For example creating a list of strings for terraform:
```yaml
inputs:
  key-1: |
    ["{{ .output.stepName.moduleName.key-1 }}", "value-1", "value-2"]
```

#### List indexes

If the parameter type is StringList then it's possible to use an index to get a specific value, e.g. `{{ .output.stepName.moduleName.key-1[0] }}` or a slice by using a range, e.g. `[0-1]`. In case of terraform output, it's also possible to use a map key as the index, e.g. `{{ .output.stepName.moduleName.key-1[key-2] }}`.

#### Escaping replacement tags

Replacement tags support escaping with inner ``{{`{{ }}`}}`` tags. For example, ``{{`{{ .dbupdate }}`}}`` will be replaced with `{{ .dbupdate }}`. This can be used to pass helm template values through the agent.

#### Optional replacement tags

If the output value is optional then use `optout` or `toptout`, it will replace the value with an empty string if the module or output is not found. Optional tag can be combined with the `|` operation to add (multiple) fallback values. Quotation marks can be used to provide a default value. For example `{{ .optout.stepName.ModuleName.key-1 | "default" }}`.

### Including files in steps

It's possible to include files in steps by adding the files into a `./config/<stepName>/include` subdirectory. File names can't include `main.tf`, `provider.tf` or `backend.conf` as they are reserved for the agent. For ArgoCD, reserved name is `argocd.yaml` and named files for every module `module-name.yaml`. Files will be copied into the step directory which is used by terraform and ArgoCD as step context.

### Including CA certificates

It's possible to include CA certificates by adding the files into a `./ca-certificates` subdirectory. Files will be copied into the bucket root and each step directory for Infralib.

### Notification API requests

When configuring api notifications, agent will send requests to the specified URL.

#### POST `/steps/status`

Status of step pipelines.
Required configured message type: `progress`
Payload example:
```json
{
  "status": "success",
  "status_at": "2021-08-31T12:00:00Z",
  "step": "net",
  "applied_at": "2021-08-31T12:00:00Z",
  "modules": [{
    "name": "net",
    "applied_version": "v1.4.2",
    "version": "v1.4.2"
  }]
}
```

* status - possible values `failure | skipped | starting | success`
* status_at - timestamp when the status was set
* step - name of the step
* error - error message if the status is failure
* applied_at - timestamp when the step was applied
* modules - list of modules
  * name - name of the module
  * applied_version - version of the module that was successfully applied previously
  * version - version of the module that was or will be applied

### Encryption

Agent uses default cloud provider encryption settings if no encryption module is present in config.

Currently, infralib only supports customer provided encryption in AWS with KMS. When KMS module is present in the config file, agent will use the KMS arn from the module terraform output to configure the S3 bucket and CloudWatch log groups to use KMS by default. Agent will also use the KMS when creating Parameter Store parameters and Secret Manager secrets. Agent applies those changes only when a previous execution has successfully applied the KMS module. Meaning, only objects that have been put in S3 after the KMS module was applied will be encrypted with it.

## Migration Helper

Agent includes 3 commands to help migrate from existing terraform state to Entigo Infralib modules: [migrate-config](#migrate-config), [migrate-plan](#migrate-plan) and [migrate-validate](#migrate-validate).

Config command requires a terraform v4 state file. Plan and validate commands require the state file and a terraform plan file. Infralib state and plan files can be obtained from the bucket used by agent. It's possible to combine the approval type `reject` with `run` command argument `steps` to generate plan files without applying them for the chosen steps. Plan files need to be manually converted into json format by using terraform.

First generate an import configuration with `migrate-config`. Modify the generated lines to remove any resources that don't need migrating. If needed, split any config items into source and destination blocks. That's only necessary if source and destination don't have any matching fields from name, module and indexes.

Using the import config, generate terraform import commands with the `migrate-plan` command. After executing the commands, run the pipelines with the approval type `reject` to generate a new plan. Optionally, use the `migrate-validate` command to validate the new plan along with the new state.

### Migration Commands

#### migrate-config

Outputs a list of resources with instance indexes based on the state file that are not matched by the provided import file. More info in [Migration Helper](#migration-helper).

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* state-file - path to the previous terraform state file [$STATE_FILE]
* import-file - **optional**, path to the import file [$IMPORT_FILE]

Example
```bash
bin/ei-agent migrate-config --state-file=state-file.json
```

#### migrate-plan

Generates import and rm terraform commands based on the input files. **Warning!** Always check the import and rm commands before executing them. More info in [Migration Helper](#migration-helper).

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* state-file - path to the previous terraform state file [$STATE_FILE]
* plan-file - path to the terraform plan file [$PLAN_FILE]
* import-file - path to the import file [$IMPORT_FILE]
* types-file - **optional**, path for type identifications file [$TYPES_FILE]

Example
```bash
bin/ei-agent migrate-plan --state-file=state-file.json --plan-file=plan.json --import-file=import-file.yaml
```

#### migrate-validate

Validate a terraform plan file based on the import config and infralib terraform state. Outputs 3 types of warnings:
1. If plan wants to create or remove a resource that should be in the infralib state file.
2. If plan wants to create or remove a resource that has the same type as the type in the import file.
3. If plan wants to change a value of a resource.

More info in [Migration Helper](#migration-helper).

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* state-file - path to the new terraform state file [$STATE_FILE]
* plan-file - path to the terraform plan file [$PLAN_FILE]
* import-file - path to the import file [$IMPORT_FILE]

Example
```bash
bin/ei-agent migrate-plan --state-file=state-file.json --import-file=import-file.yaml
```

### Import File

Import configuration file is used to tell the migration commands which type of resources should be migrated.

```yaml
import:
  - type: string
    name: string
    module: string
    index_keys: []int | []string
    source:
      module: string
      name: string
      index_key: int | string
      index_keys: []int | []string
    destination:
      module: string
      name: string
      index_key: int | string
      index_keys: []int | []string
```

* type - type of the resource to import
* name - optional, overwrites source and destination names
* module - optional, overwrites source and destination modules
* index_keys - optional, overwrites source and destination index_keys
* source - source is a resource from the previous state file
* destination - destination is a resource from the plan file
    * module - optional, module name of the resource
    * name - optional, name of the resource
    * index_key - optional, index key of the resource instance
    * index_keys - optional, index keys of the resource instance

If module and name are empty then agent will try to find the resource from the state/plan file based on the type. If there are more than 1 resource with that type then a warning is printed. Agent will generate import commands for all resources of that type.

Index key and index keys are mutually exclusive. Index keys can be used to map instances in different order to the new state. If index hasn't been set, then agent will use keys from state resource instances.

Plan command generates import commands for the destination resources and rm commands for the source resources.

### Type Identifications File

Type identifications file is used to map a resource type to an identification used by terraform for the import command. Agent will replace any placeholder `{}` values with values from the resource instance attributes. Default file is located in `migrate/types.yaml`.

```yaml
typeIdentifications:
  - identification: string
    listSeparator: string
    types: []string
```

* identification - identification used by terraform for the import command, e.g `"{id}"`
* listSeparator - optional, separator for identification values that are lists, default `/`
* types - list of resource types that use the identification

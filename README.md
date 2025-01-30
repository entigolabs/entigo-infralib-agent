# Entigo Infralib Agent

Entigo infralib agent prepares an AWS Account or Google Cloud Project for Entigo infralib terraform modules.
Creates the required resources for S3/storage, DynamoDB, CloudWatch, CodeBuild/Cloud Run Jobs, CodePipeline/Delivery Pipeline, and IAM roles and policies.
Executes pipelines which apply the specified Entigo infralib terraform modules. During subsequent runs, the agent will update the modules to the latest version and apply any config changes.

* [Requirements](#requirements)
* [Compiling Source](#compiling-source)
* [Installation](#installation)
* [Docker](#docker)
    * [Building a local Docker image](#building-a-local-docker-image)
    * [Running the Docker image](#running-the-docker-image)
* [Commands](#commands)
    * [Bootstrap](#bootstrap)
    * [Run](#run)
    * [Update](#update)
    * [Delete](#delete)
    * [Service Account](#service-account)
    * [Pull](#pull)
    * [Migrate Plan](#migrate-plan)
* [Config](#config)
  * [Auto approval logic](#auto-approval-logic)
  * [Overriding config values](#overriding-config-values)
  * [Including files in steps](#including-files-in-steps)

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
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - **optional** role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* github-token - **optional** GitHub token for querying releases as unauthenticated rate limit is low [$GITHUB_TOKEN]
* steps - **optional** comma separated list of steps to run [$STEPS]
* allow-parallel - allow running steps in parallel on first execution cycle (default: **true**) [$ALLOW_PARALLEL]
* pipeline-type - pipeline execution type (local | cloud), local is meant to be run inside the infralib image (default: **cloud**) [$PIPELINE_TYPE]
* print-logs - print terraform/helm logs to stdout when using local execution (default: **true**) [$PRINT_LOGS]
* logs-path - **optional** path for storing terraform/helm logs when running local pipelines [$LOGS_PATH]

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
* prefix - prefix used when creating cloud resources (default: **config prefix**) [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - **optional** role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]
* github-token - **optional** GitHub token for querying releases as unauthenticated rate limit is low [$GITHUB_TOKEN]
* steps - **optional** comma separated list of steps to run [$STEPS]
* pipeline-type - pipeline execution type (local | cloud), local is meant to be run inside the infralib image (default: **cloud**) [$PIPELINE_TYPE]
* print-logs - print terraform/helm logs to stdout when using local execution (default: **true**) [$PRINT_LOGS]
* logs-path - **optional** path for storing terraform/helm  logs when running local pipelines [$LOGS_PATH]

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

OPTIONS:
* prefix - prefix used when creating cloud resources [$PREFIX]
* project-id - project id used when creating gcloud resources [$PROJECT_ID]
* location - location used when creating gcloud resources [$LOCATION]
* zone - zone used in gcloud run jobs [$ZONE]
* role-arn - role arn for assume role, used when creating aws resources in external account [$ROLE_ARN]

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

### migrate-plan

Compile a migration plan for terraform.

OPTIONS:
* logging - logging level (debug | info | warn | error) (default: **info**) [$LOGGING]
* state-file - path to the terraform state file [$STATE_FILE]
* plan-file - path to the terraform plan file [$PLAN_FILE]
* import-file - path to the import file [$IMPORT_FILE]
* types-file - **optional**, path for type identifications file [$TYPES_FILE]

Example
```bash
bin/ei-agent migrate-plan --state-file=state-file.json --import-file=import-file.yaml
```

## Config

Config is provided with a yaml file:

```yaml
prefix: string
sources:
  - url: https://github.com/entigolabs/entigo-infralib-release | path
    version: stable | semver
    include: []string
    exclude: []string
    force_version: bool
destinations:
  - name:
    git:
      url: string
      key: string
      key_password: string
      username: string
      password: string
      author_name: string
      author_email: string
      insecure: bool
agent_version: latest | semver
base_image_source: string
base_image_version: stable | semver
steps:
  - name: string
    type: terraform | argocd-apps
    approve: minor | major | never | always | force | reject
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
* agent_version - image version of Entigo Infralib Agent to use
* base_image_source - source of Entigo Infralib Base Image to use
* base_image_version - image version of Entigo Infralib Base Image to use, default uses the version from step
* steps - list of steps to execute
  * name - name of the step
  * type - type of the step
  * approve - approval type for the step, possible values `minor | major | never | always | force | reject`, default **always**. More info in [Auto approval logic](#auto-approval-logic)
  * base_image_source - source of Entigo Infralib Base Image to use
  * base_image_version - image version of Entigo Infralib Base Image to use, default uses the newest module version
  * vpc - vpc values to add
    * attach - attach vpc to code build/cloud run job, if other fields are empty then uses default vpc based on typed output of a vpc module, default **nil**. When nil, the value will be set based on the step type
    * id - vpc id for code build/cloud run job
    * subnet_ids - vpc subnet ids for code build/cloud run job
    * security_group_ids - vpc security group ids for code build/cloud run job
  * kubernetes_cluster_name - kubernetes cluster name for argocd-apps steps
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

### Auto approval logic

Each step can set an approval type which lets agent decide when to auto approve pipeline changes. Auto approve type is only considered when resources will be changed. Adding resources doesn't require manual approval. Destroying resources always requires manual approval, except when using type `force`. Approve always means that manual approval is required, never means that agent approves automatically. Major and minor types require manual approval only when any of the step modules has a major or minor semver version change. Modules with external source require manual approval. If the planning stage of a step finds no changes, then the pipeline apply stage will be skipped.

It's possible to use the type `reject` to stop the pipeline instead of approving. This can be used to generate plan files without applying them. Agent marks the step as failed.

### Overriding config values

Step, module and input field values can be overwritten by using replacement tags `{{ }}`.

Replacement tags can be overwritten by values from terraform output, config itself or custom agent logic. If the value is not found from terraform output, then the value is requested from AWS SSM Parameter Store or Google Cloud Secret Manager. For output values it's possible to use the keywords `ssm`, `gcsm` and `output`. There's also a special type based keyword `toutput` that uses the output from a module with the specified type instead of a name.

If the output value is optional then use `optout` or `toptout`, it will replace the value with an empty string if the module or output is not found.

Replacement tags support escaping with inner ``{{`{{ }}`}}`` tags. For example, ``{{`{{ .dbupdate }}`}}`` will be replaced with `{{ .dbupdate }}`. This can be used to pass helm template values through the agent.

For example, `{{ .ssm.stepName.moduleName.key-1 }}` will be overwritten with the value from terraform output `moduleName__key-1`. As a fallback, uses SSM Parameter Store parameter `/entigo-infralib/config.prefix-stepName-moduleName-parentStep/key-1`.
If the parameter type is StringList then it's possible to use an index to get a specific value, e.g. `{{ .ssm.stepName.moduleName.key-1[0] }}` or a slice by using a range, e.g. `[0-1]`.

It's possible to build a custom array by using yaml multiline string, even mixing replaced values with inputted values. For example creating a list of strings for terraform:
```yaml
inputs:
  key-1: |
    ["{{ .ssm.stepName.moduleName.key-1 }}", "value-1", "value-2"]
```

Custom SSM parameter example `{{ .ssm-custom.key }}` will be overwritten by the value of the custom SSM parameter `key`.
For custom GCloud SM, replace the ssm with gcsm. For universal output, replace the ssm with output `output-custom`.

Config example `{{ .config.prefix }}` will be overwritten by the value of the config field `prefix`. Config replacement does not support indexed paths.

Agent example `{{ .agent.version.step.module }}` will be overwritten by the value of the specified module version that's currently being applied or a set version, e.g `v0.8.4`. Agent replacement also supports account id using key accountId.

Infralib modules may use `{{ .tmodule.type }}` in their default input files to replace it with the name of the module used in the config. Alternatively, modules may use `{{ .tsmodule.type }}` to replace it with the name of the typed module used in the active step. It's also possible to use `{{ .module.name  }}` and `{{ .module.source }}` to replace them with module name and source, but those tags only exclusively apply for module inputs, including all input files.

### Including files in steps

It's possible to include files in steps by adding the files into a `./config/<stepName>/include` subdirectory. File names can't include `main.tf`, `provider.tf` or `backend.conf` as they are reserved for the agent. For ArgoCD, reserved name is `argocd.yaml` and named files for every module `module-name.yaml`. Files will be copied into the step directory which is used by terraform and ArgoCD as step context.
# entigo-infralib-agent

Entigo infralib agent prepares an AWS Account for Entigo infralib terraform modules.
Creates the required resources for S3, DynamoDB, CloudWatch, CodeBuild, CodePipeline, and IAM roles and policies.
Executes CodePipelines which apply the specified Entigo infralib terraform modules.

* [Compiling Source](#compiling-source)
* [Requirements](#requirements)
* [Docker](#docker)
    * [Building a local Docker image](#building-a-local-docker-image)
    * [Running the Docker image](#running-the-docker-image)
* [Commands](#commands)
    * [Bootstrap](#bootstrap)
    * [Run](#run)
    * [Merge](#merge)
* [Config](#config)
  * [Overriding config values](#overriding-config-values)

## Compiling Source

```go build -o bin/ei-agent main.go```

## Requirements

AWS Service Account with administrator access, credentials provided by AWS or environment variables.

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

### bootstrap

Creates the required AWS resources and a codepipeline for executing the agent. If the pipeline already exists, the agent image version will be updated if needed and a new execution will be started.

OPTIONS:
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* branch - CodeCommit branch name (default: **main**) [$BRANCH]
* aws-prefix - prefix used when creating aws resources (default: **entigo-infralib**) [$AWS_PREFIX]

Example
```bash
bin/ei-agent bootstrap --config=config.yaml --branch=main --aws-prefix=entigo-infralib
```

### run

Processes config steps, creates and executes CodePipelines which apply Entigo Infralib terraform modules.

OPTIONS:
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* branch - CodeCommit branch name (default: **main**) [$BRANCH]
* aws-prefix - prefix used when creating aws resources (default: **entigo-infralib**) [$AWS_PREFIX]

Example
```bash
bin/ei-agent run --config=config.yaml --branch=main --aws-prefix=entigo-infralib
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
steps:
  - name: string
    type: terraform | argocd-apps | terraform-custom 
    workspace: string
    before: string
    approve: minor | major | never | always
    version: stable | semver
    remove: bool
    vpc_id: string
    vpc_subnet_ids: multiline string
    vpc_security_group_ids: multiline string
    repo_url: string
    modules:
      - name: string
        source: string
        version: stable | semver
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
* prefix - prefix used for CodeCommit folders/files and terraform resources
* source - source repository for Entigo Infralib terraform modules
* version - version of Entigo Infralib terraform modules to use
* agent_version - image version of Entigo Infralib Agent to use
* steps - list of steps to execute
  * name - name of the step
  * type - type of the step
  * workspace - terraform workspace to use
  * before - for patch config, name of the step in the same workspace that this step should be executed before
  * approve - approval type for the step, only applies when terraform needs to change or destroy resources, based on semver. Approve always means that manual approval is required, never means that agent approves automatically, default **always**
  * version - version of Entigo Infralib terraform modules to use
  * remove - whether to remove the step during merge or not, default **false**
  * vpc_id - vpc id for code build
  * vpc_subnet_ids - vpc subnet ids for code build
  * vpc_security_group_ids - vpc security group ids for code build
  * repo_url - for argocd-apps steps, repo to use for cloning
  * modules - list of modules to apply
    * name - name of the module
    * source - source of the terraform module
    * version - version of the module to use
    * remove - whether to remove the module during merge or not, default **false**
    * inputs - map of inputs for the module, string values need to be quoted
  * provider - provider values to add
    * inputs - variables for provider tf file
    * aws - aws provider default and ignore tags to add
    * kubernetes - kubernetes provider ignore annotations and labels to add

### Overriding config values

Step, module and input field values can be overwritten by using replacement tags `{{ }}`.

Replacement tags can be overwritten by values that are stored in the AWS SSM Parameter Store, config itself or custom agent logic.

For example, `{{ .ssm.stepName.moduleName.key-1/key-2 }}` will be overwritten by the value of the SSM Parameter Store parameter `/entigo-infralib/config.prefix-stepName-moduleName-parentStep.workspace/key-1/key-2`.

Config example `{{ .config.prefix }}` will be overwritten by the value of the config field `prefix`. Config replacement does not support indexed paths.

Agent example `{{ .agent.version.step.module }}` will be overwritten by the value of the specified module version that's currently being applied or a set version, e.g `v0.8.4`.
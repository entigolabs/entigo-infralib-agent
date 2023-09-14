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
* [Config](#config)

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

```docker run --pull -it --rm -v "$(pwd)/config.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent```

To execute the [bootstrap](#bootstrap), override the default command.

```docker run --pull -it --rm -v "$(pwd)/config.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent ei-agent bootstrap```

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
    approve: minor | major | never | always
    version: stable | semver
    remove: bool
    vpc_prefix: string
    argocd_prefix: string
    modules:
      - name: string
        source: string
        version: stable | semver
        remove: bool
        inputs: map[string]string
```
Config version is overwritten by step version which in turn is overwritten by module version. Default version is **stable**.

* base_config - base config, pulled from source
  * version - version of Entigo Infralib base config
  * profile - name of the config file without a suffix, empty string means no base config is used
* prefix - prefix used for CodeCommit folders/files and terraform resources
* source - source repository for Entigo Infralib terraform modules
* version - version of Entigo Infralib terraform modules to use
* agent_version - image version of Entigo Infralib Agent to use
* steps - list of steps to execute
  * name - name of the step
  * type - type of the step
  * workspace - terraform workspace to use
  * approve - approval type for the step, only applies when terraform needs to change or destroy resources, based on semver
  * version - version of Entigo Infralib terraform modules to use
  * remove - whether to remove the step during merge or not, default **false**
  * vpc_prefix - whether to attach a vpc to codebuild or not, used for getting vpc config from SSM
  * argocd_prefix - for argocd-apps steps for getting a repoUrl which will be used for cloning
  * modules - list of modules to apply
    * name - name of the module
    * source - source of the terraform module
    * version - version of the module to use
    * remove - whether to remove the step during merge or not, default **false**
    * inputs - map of inputs for the module, string values need to be quoted, complex values need to be as multiline strings with |

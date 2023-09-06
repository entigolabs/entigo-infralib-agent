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

```docker run -it --rm -v "$(pwd)/config.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent```

To execute the [bootstrap](#bootstrap), override the default command.

```docker run -it --rm -v "$(pwd)/config.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent ei-agent bootstrap```

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
# entigo-infralib-agent

Entigo infralib agent prepares AWS Account for Entigo infralib usage.
Creating required resources for S3, DynamoDB, CloudWatch, CodeBuild, CodePipeline, IAM roles and policies.

## Compiling Source

```go build -o bin/ei-agent main.go```

## Requirements

AWS Service Account with administrator access, credentials provided by AWS or environment variables.

## Docker

```docker build -t entigolabs/entigo-infralib-agent .```

```docker run -it --rm -v "$(pwd)/config.yaml":"/etc/ei-agent/config.yaml" -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY -e AWS_REGION=$AWS_REGION -e AWS_SESSION_TOKEN=$AWS_SESSION_TOKEN -e CONFIG=/etc/ei-agent/config.yaml entigolabs/entigo-infralib-agent```

## Commands

### Run

Runs the agent

OPTIONS:
* config - config file path and name, only needed for first run or when overriding an existing config [$CONFIG]
* branch - CodeCommit branch name (default: main) [$BRANCH]
* aws-prefix - prefix used when creating aws resources (default: entigo-infralib) [$AWS_PREFIX]
* init-version - initial version to install, only applies to first run [$INIT_VERSION]
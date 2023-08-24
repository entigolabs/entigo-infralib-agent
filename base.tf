terraform {
  backend "s3" {}
  required_version = ">= 1.4"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">4"
    }
  }
}

provider "aws" {
  ignore_tags {
      key_prefixes = ["kubernetes.io/cluster/"]
  }
}


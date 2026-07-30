terraform {
  source = "git::git@github.com:transcend-io/terraform-aws-fargate-container?ref=v0.0.4"
}

locals {
  origdir = read_terragrunt_config("../origdir.hcl")
}

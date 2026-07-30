terraform {
  source = "git::git@github.com:transcend-io/terraform-aws-fargate-container?ref=v0.0.4"
}

locals {
  shared = read_terragrunt_config("../shared.hcl")
}

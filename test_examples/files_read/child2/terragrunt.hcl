terraform {
  source = "git::git@github.com:transcend-io/terraform-aws-fargate-container?ref=v0.0.4"
}

locals {
  parent = find_in_parent_folders("common.hcl")
  shared = read_terragrunt_config("../shared.hcl")
}

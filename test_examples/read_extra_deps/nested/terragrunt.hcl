terraform {
  source = "git::git@github.com:transcend-io/terraform-aws-fargate-container?ref=v0.0.4"
}

locals {
  mid = read_terragrunt_config("../mid.hcl")
}

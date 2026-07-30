terraform {
  source = "git::git@github.com:transcend-io/terraform-aws-fargate-container?ref=v0.0.4"
}

# file()/templatefile()/find_in_parent_folders() targets are deliberately absent from
# this project's when_modified: terragrunt does not record them as read, and see
# cmd/read_terragrunt_config.go for why this tool cannot record them either.
locals {
  data     = file("../data.json")
  rendered = templatefile("../tmpl.tftpl", {})
  parent   = find_in_parent_folders("common.hcl")
  shared   = read_terragrunt_config("../shared.hcl")
}

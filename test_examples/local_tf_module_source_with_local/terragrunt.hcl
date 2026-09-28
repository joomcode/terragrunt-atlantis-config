terraform {
}

generate "root_module_path" {
  path      = "root_module_path.tf"
  if_exists = "overwrite_terragrunt"
  contents  = <<EOF
locals {
  root_module_path = "${join("", [for _ in range(20) : "../"])}${get_parent_terragrunt_dir()}"
}
EOF
}

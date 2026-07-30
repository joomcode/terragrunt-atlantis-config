# Shared config read via read_terragrunt_config. Its extra_atlantis_dependencies must
# reach every consuming unit even though terragrunt never reads these files: they are the
# kind a terraform module consumes at apply. Relative entries resolve against *this*
# file's directory, so `direct` gets ../assets/x.json, not direct/assets/x.json.
locals {
  extra_atlantis_dependencies = [
    "assets/x.json",
    "assets/*.conf",
    "${get_terragrunt_dir()}/assets/abs.json",
  ]
}

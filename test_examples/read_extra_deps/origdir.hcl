# Declares a dependency relative to the *consuming* unit via get_original_terragrunt_dir().
# The read memo must therefore key this target per consuming unit: per_unit_a has to get
# per_unit_a/local.json and per_unit_b per_unit_b/local.json, not whichever unit parsed
# the target first.
locals {
  extra_atlantis_dependencies = [
    "${get_original_terragrunt_dir()}/local.json",
  ]
}

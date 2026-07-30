# Shared config read via read_terragrunt_config from multiple children. It reads a
# per-stack file using get_original_terragrunt_dir() (the consuming stack's dir), so
# its transitive read set is caller-dependent: child must pull in child/stack.hcl and
# child2 must pull in child2/stack.hcl. The read memo therefore keys this target per
# consuming stack dir rather than sharing one read set across both.
locals {
  stack = read_terragrunt_config("${get_original_terragrunt_dir()}/stack.hcl")
}

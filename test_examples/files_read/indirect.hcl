# Reads shared.hcl (the get_original_terragrunt_dir consumer) without mentioning that
# function itself: guards that caller-dependence is detected transitively — child3 must
# get child3/stack.hcl even though the consumer sits one nested read below its target.
locals {
  shared = read_terragrunt_config("shared.hcl")
}

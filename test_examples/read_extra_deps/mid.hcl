# Declares nothing itself and only reads deep.hcl: guards that a declaration one nested
# read below the unit's own target still lands in when_modified.
locals {
  deep = read_terragrunt_config("deep.hcl")
}

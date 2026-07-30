# A read target whose extra_atlantis_dependencies list is not all strings: the error must
# surface instead of being silently dropped.
locals {
  extra_atlantis_dependencies = [
    "fine.json",
    42,
  ]
}

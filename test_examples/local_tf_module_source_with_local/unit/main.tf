module "local_module" {
  source = "../modules/local-module"
}

module "root_module" {
  source = "${local.root_module_path}//modules/root-module"
}

module "unknown_module" {
  source = "${local.unknown_path}//modules/unknown-module"
}

locals {
  names = [for name in ["a", "b"] : upper(name)]
}

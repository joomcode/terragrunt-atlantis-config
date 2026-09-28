module "local_module" {
  source = "../modules/local-module"
}

module "root_module" {
  source = "${local.root_module_path}//modules/root-module"
}

locals {
  names = [for name in ["a", "b"] : upper(name)]
}

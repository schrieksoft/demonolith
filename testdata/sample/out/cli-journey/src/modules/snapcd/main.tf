terraform {
  required_providers {
    snapcd = {
      source = "registry.terraform.io/schrieksoft/snapcd"
    }
  }
}

provider "snapcd" {
  client_id            = var.client_id
  client_secret        = var.client_secret
  organization_id      = var.organization_id
  url                  = var.snapcd_server_url
  insecure_skip_verify = var.insecure_skip_verify
}

data "snapcd_stack" "this" {
  name = var.stack_name
}

data "snapcd_runner" "this" {
  name = var.runner_name
}

resource "snapcd_namespace" "this" {
  name           = var.namespace_name
  stack_id       = data.snapcd_stack.this.id
  default_engine = var.engine
}

resource "snapcd_module" "app" {
  name                = "app"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/app"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module" "cluster" {
  name                = "cluster"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/cluster"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module" "database" {
  name                = "database"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/database"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module" "legacy" {
  name                = "legacy"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/legacy"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module" "networking" {
  name                = "networking"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/networking"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module_input_from_output" "app_module_cluster_cluster_endpoint" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "module_cluster_cluster_endpoint"
  output_module_id = snapcd_module.cluster.id
  output_name      = "module_cluster_cluster_endpoint"
}

resource "snapcd_module_input_from_output" "app_module_cluster_cluster_id" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "module_cluster_cluster_id"
  output_module_id = snapcd_module.cluster.id
  output_name      = "module_cluster_cluster_id"
}

resource "snapcd_module_input_from_output" "app_module_database" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "module_database"
  output_module_id = snapcd_module.database.id
  output_name      = "module_database"
}

resource "snapcd_module_input_from_output" "app_random_pet_network_name" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "random_pet_network_name"
  output_module_id = snapcd_module.networking.id
  output_name      = "random_pet_network_name"
}

resource "snapcd_module_input_from_output" "cluster_module_private_subnet_subnet_id" {
  input_kind       = "Param"
  module_id        = snapcd_module.cluster.id
  name             = "module_private_subnet_subnet_id"
  output_module_id = snapcd_module.networking.id
  output_name      = "module_private_subnet_subnet_id"
}

resource "snapcd_module_input_from_output" "cluster_module_public_subnet" {
  input_kind       = "Param"
  module_id        = snapcd_module.cluster.id
  name             = "module_public_subnet"
  output_module_id = snapcd_module.networking.id
  output_name      = "module_public_subnet"
}

resource "snapcd_module_input_from_output" "cluster_random_uuid_vpc_id" {
  input_kind       = "Param"
  module_id        = snapcd_module.cluster.id
  name             = "random_uuid_vpc_id"
  output_module_id = snapcd_module.networking.id
  output_name      = "random_uuid_vpc_id"
}

resource "snapcd_module_input_from_output" "database_module_private_subnet_cidr_block" {
  input_kind       = "Param"
  module_id        = snapcd_module.database.id
  name             = "module_private_subnet_cidr_block"
  output_module_id = snapcd_module.networking.id
  output_name      = "module_private_subnet_cidr_block"
}

resource "snapcd_module_input_from_output" "database_module_private_subnet_subnet_id" {
  input_kind       = "Param"
  module_id        = snapcd_module.database.id
  name             = "module_private_subnet_subnet_id"
  output_module_id = snapcd_module.networking.id
  output_name      = "module_private_subnet_subnet_id"
}

resource "snapcd_depends_on_module" "cluster_on_networking" {
  module_id            = snapcd_module.cluster.id
  depends_on_module_id = snapcd_module.networking.id
}


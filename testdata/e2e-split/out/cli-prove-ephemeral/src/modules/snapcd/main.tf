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

resource "snapcd_module" "data" {
  name                = "data"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/data"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module" "monolith" {
  name                = "monolith"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/monolith"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module" "network" {
  name                = "network"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/network"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module_input_from_output" "app_module_idgen" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "module_idgen"
  output_module_id = snapcd_module.network.id
  output_name      = "module_idgen"
}

resource "snapcd_module_input_from_output" "app_random_integer_net_id" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "random_integer_net_id"
  output_module_id = snapcd_module.network.id
  output_name      = "random_integer_net_id"
}

resource "snapcd_module_input_from_output" "app_random_pet_fp_tag" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "random_pet_fp_tag"
  output_module_id = snapcd_module.data.id
  output_name      = "random_pet_fp_tag"
}

resource "snapcd_module_input_from_output" "app_random_pet_net_name" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "random_pet_net_name"
  output_module_id = snapcd_module.network.id
  output_name      = "random_pet_net_name"
}

resource "snapcd_module_input_from_output" "app_random_string_token" {
  input_kind       = "Param"
  module_id        = snapcd_module.app.id
  name             = "random_string_token"
  output_module_id = snapcd_module.data.id
  output_name      = "random_string_token"
}

resource "snapcd_module_input_from_output" "network_random_string_token" {
  input_kind       = "Param"
  module_id        = snapcd_module.network.id
  name             = "random_string_token"
  output_module_id = snapcd_module.data.id
  output_name      = "random_string_token"
}


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

resource "snapcd_module" "a" {
  name                = "a"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/a"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

resource "snapcd_module" "b" {
  name                = "b"
  namespace_id        = snapcd_namespace.this.id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}modules/b"
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

resource "snapcd_module_input_from_output" "b_random_integer_seed" {
  input_kind       = "Param"
  module_id        = snapcd_module.b.id
  name             = "random_integer_seed"
  output_module_id = snapcd_module.a.id
  output_name      = "random_integer_seed"
}


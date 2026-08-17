terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.6.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.0.5"
    }
    time = {
      source  = "hashicorp/time"
      version = "0.11.2"
    }
    snapcd = {
      source  = "registry.terraform.io/schrieksoft/snapcd"
      version = "1.5.0"
    }
  }
}

provider "snapcd" {
  client_id            = "default"
  client_secret        = "default"
  organization_id      = "10000000-0000-0000-0000-000000000000"
  url                  = "http://localhost:5000"
  insecure_skip_verify = true
}

variable "database_port" {
  description = "Port the database listens on"
  type        = number
  default     = 5432
}

variable "name_prefix" {
  description = "Prefix for every named resource in the deployment"
  type        = string
  default     = "acme"
}

variable "resource_group_name" {
  description = "Resource group the whole deployment lands in"
  type        = string
  default     = "acme-prod"
}

locals {
  database_name = "${local.name}-orders"
  environment   = "e2e"
  name          = "${var.name_prefix}-${local.environment}"
}

# Deployment context, read from the Snap CD server. Consumed by the
# networking, database, cluster, and app sections below.
data "snapcd_stack" "default" {
  name = "default"
}

# An external module, pulled straight from GitHub (not a Snap CD module).
module "database" {
  source              = "github.com/snapcd-samples/mock-module-database"
  resource_group_name = var.resource_group_name
  database_name       = local.database_name
  database_sku        = "db.t3.small"
  deploy_to_subnet_id = var.module_private_subnet_subnet_id
}

resource "random_uuid" "database_firewall_rule" {
  keepers = {
    subnet_cidr = var.module_private_subnet_cidr_block
    port        = tostring(var.database_port)
    stack       = data.snapcd_stack.default.id
  }
}


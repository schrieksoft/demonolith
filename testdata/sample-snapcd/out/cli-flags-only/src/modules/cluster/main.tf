provider "snapcd" {
  client_id            = "default"
  client_secret        = "default"
  organization_id      = "10000000-0000-0000-0000-000000000000"
  url                  = "http://localhost:5000"
  insecure_skip_verify = true
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
  cluster_name = "${local.name}-cluster"
  environment  = "e2e"
  name         = "${var.name_prefix}-${local.environment}"
}

data "snapcd_runner" "default" {
  name = "default"
}

# Also external, also from GitHub.
module "cluster" {
  source              = "github.com/snapcd-samples/mock-module-kubernetes-cluster"
  resource_group_name = var.resource_group_name
  cluster_name        = local.cluster_name
  vpc_id              = var.random_uuid_vpc_id
  public_subnet_id    = var.module_public_subnet
  private_subnet_id   = var.module_private_subnet_subnet_id
  kubernetes_version  = "1.28"
  node_instance_type  = "m5.large"
  desired_capacity    = 2
}

resource "random_pet" "node_pool" {
  prefix = local.cluster_name
  keepers = {
    nodes  = "2"
    runner = data.snapcd_runner.default.id
  }
}


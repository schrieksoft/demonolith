variable "environment_json" {
  description = "Environment settings as a JSON string (the content of config/environment.json)"
  type        = string
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
  environment  = local.settings.environment
  name         = "${var.name_prefix}-${local.environment}"
  settings     = jsondecode(var.environment_json)
}

# Also external-style, also a local source.
module "cluster" {
  source              = "./modules/mock-cluster"
  resource_group_name = var.resource_group_name
  cluster_name        = local.cluster_name
  vpc_id              = var.random_uuid_vpc_id
  public_subnet_id    = var.module_public_subnet
  private_subnet_id   = var.module_private_subnet_subnet_id
  kubernetes_version  = local.settings.cluster.kubernetes_version
  node_instance_type  = local.settings.cluster.node_instance_type
  desired_capacity    = local.settings.cluster.node_count
}

resource "random_pet" "node_pool" {
  prefix = local.cluster_name
  keepers = {
    version = local.settings.cluster.kubernetes_version
    nodes   = tostring(local.settings.cluster.node_count)
  }
}


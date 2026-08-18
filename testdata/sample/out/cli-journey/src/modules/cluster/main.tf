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
  settings     = jsondecode(data.local_file.environment.content)
}

# Environment settings, read from a checked-in file and decoded in locals.tf.
# Consumed by the database, cluster, and app sections below.
data "local_file" "environment" {
  filename = "${path.module}/config/environment.json"
}

# Also external, also from GitHub.
module "cluster" {
  source              = "github.com/snapcd-samples/mock-module-kubernetes-cluster"
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


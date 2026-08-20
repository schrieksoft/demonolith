variable "database_port" {
  description = "Port the database listens on"
  type        = number
  default     = 5432
}

variable "environment_json" {
  description = "Environment settings as a JSON string (the content of config/environment.json)"
  type        = string
}

variable "name_prefix" {
  description = "Prefix for every named resource in the deployment"
  type        = string
  default     = "acme"
}

locals {
  app_name               = "${local.name}-${local.settings.app.name}"
  deploy_key_fingerprint = data.tls_public_key.deploy_key.public_key_fingerprint_md5
  environment            = local.settings.environment
  name                   = "${var.name_prefix}-${local.environment}"
  network_zone           = "${var.random_pet_network_name}.internal"
  settings               = jsondecode(var.environment_json)
}

data "tls_public_key" "deploy_key" {
  private_key_pem = tls_private_key.deploy_signer.private_key_pem
}

module "storefront_dns" {
  source = "../dns-record"
  zone   = local.network_zone
  name   = local.app_name
  target = var.module_cluster_cluster_endpoint
}

resource "random_password" "app_session_secret" {
  length  = 24
  special = false
}

resource "random_pet" "app_release" {
  prefix = local.app_name
  keepers = {
    cluster     = var.module_cluster_cluster_id
    db_endpoint = var.module_database
    db_port     = tostring(var.database_port)
    replicas    = tostring(local.settings.app.replicas)
    deploy_key  = local.deploy_key_fingerprint
    zone        = local.network_zone
  }
}

# The deployment signing key; its public half is read back through a data
# source and referenced by the app section. The key lives with the app: its
# PEM is provider-sensitive, and a sensitive value must not cross a carve
# boundary (see demonolith's LIMITATIONS.md), so everything that reads it is
# placed together.
resource "tls_private_key" "deploy_signer" {
  algorithm = "ED25519"
}


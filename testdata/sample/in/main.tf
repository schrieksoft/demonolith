# ===========================================================================
# The monolith.
#
# One root, one state, everything in it: networking, a database, a Kubernetes
# cluster, and the app on top — plus the shared plumbing (a config-file data
# source, a deploy key, locals) that all of them read. Every resource is a
# mock (random/tls providers), so this applies anywhere with no cloud
# credentials; the reference structure is the point, not the infrastructure.
# ===========================================================================

# --- shared plumbing -------------------------------------------------------

# Environment settings, read from a checked-in file and decoded in locals.tf.
# Consumed by the database, cluster, and app sections below.
data "local_file" "environment" {
  filename = "${path.module}/config/environment.json"
}

# The deployment signing key; its public half is read back through a data
# source and referenced by the app section. The key lives with the app: its
# PEM is provider-sensitive, and a sensitive value must not cross a carve
# boundary (see demonolith's LIMITATIONS.md), so everything that reads it is
# placed together.
# @demono:move app
resource "tls_private_key" "deploy_signer" {
  algorithm = "ED25519"
}

data "tls_public_key" "deploy_key" {
  private_key_pem = tls_private_key.deploy_signer.private_key_pem
}

# --- networking ------------------------------------------------------------

# @demono:move networking
resource "random_uuid" "vpc_id" {}

# @demono:move networking
resource "random_pet" "network_name" {
  prefix = var.name_prefix
  length = 2
}

# @demono:move networking
module "public_subnet" {
  source     = "./modules/subnet"
  vpc_id     = random_uuid.vpc_id.result
  cidr_block = var.public_subnet_cidr
  name       = "${local.name}-public"
}

# @demono:move networking
module "private_subnet" {
  source     = "./modules/subnet"
  vpc_id     = random_uuid.vpc_id.result
  cidr_block = var.private_subnet_cidr
  name       = "${local.name}-private"
}

# @demono:move networking
resource "random_uuid" "nat_gateway_id" {
  keepers = {
    vpc = random_uuid.vpc_id.result
  }
}

# --- database --------------------------------------------------------------

# An external-style module (local source so tests stay offline and fast).
# @demono:move database
module "database" {
  source              = "./modules/mock-database"
  resource_group_name = var.resource_group_name
  database_name       = local.database_name
  database_sku        = local.settings.database.sku
  deploy_to_subnet_id = module.private_subnet.subnet_id
}

# @demono:move database
resource "random_uuid" "database_firewall_rule" {
  keepers = {
    subnet_cidr = module.private_subnet.cidr_block
    port        = tostring(var.database_port)
  }
}

# --- cluster ---------------------------------------------------------------

# Also external-style, also a local source.
# @demono:move cluster
module "cluster" {
  source              = "./modules/mock-cluster"
  resource_group_name = var.resource_group_name
  cluster_name        = local.cluster_name
  vpc_id              = random_uuid.vpc_id.result
  public_subnet_id    = module.public_subnet.subnet_id
  private_subnet_id   = module.private_subnet.subnet_id
  kubernetes_version  = local.settings.cluster.kubernetes_version
  node_instance_type  = local.settings.cluster.node_instance_type
  desired_capacity    = local.settings.cluster.node_count
  depends_on          = [random_uuid.nat_gateway_id]
}

# @demono:move cluster
resource "random_pet" "node_pool" {
  prefix = local.cluster_name
  keepers = {
    version = local.settings.cluster.kubernetes_version
    nodes   = tostring(local.settings.cluster.node_count)
  }
}

# --- app -------------------------------------------------------------------

# @demono:move app
resource "random_password" "app_session_secret" {
  length  = 24
  special = false
}

# @demono:move app
resource "random_pet" "app_release" {
  prefix = local.app_name
  keepers = {
    cluster     = module.cluster.cluster_id
    db_endpoint = module.database.database_endpoint
    db_port     = tostring(var.database_port)
    replicas    = tostring(local.settings.app.replicas)
    deploy_key  = local.deploy_key_fingerprint
    zone        = local.network_zone
  }
}

# @demono:move app
module "storefront_dns" {
  source = "./modules/dns-record"
  zone   = local.network_zone
  name   = local.app_name
  target = module.cluster.cluster_endpoint
}

# --- ops odds and ends -----------------------------------------------------

resource "random_uuid" "audit_log_bucket_id" {}

resource "random_pet" "backup_plan" {
  length = 2
}

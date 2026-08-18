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

variable "private_subnet_cidr" {
  description = "CIDR block for the private subnet"
  type        = string
  default     = "10.0.2.0/24"
}

variable "public_subnet_cidr" {
  description = "CIDR block for the public subnet"
  type        = string
  default     = "10.0.1.0/24"
}

locals {
  environment = "e2e"
  name        = "${var.name_prefix}-${local.environment}"
}

# Deployment context, read from the Snap CD server. Consumed by the
# networking, database, cluster, and app sections below.
data "snapcd_stack" "default" {
  name = "default"
}

module "private_subnet" {
  source     = "./modules/subnet"
  vpc_id     = random_uuid.vpc_id.result
  cidr_block = var.private_subnet_cidr
  name       = "${local.name}-private"
}

module "public_subnet" {
  source     = "./modules/subnet"
  vpc_id     = random_uuid.vpc_id.result
  cidr_block = var.public_subnet_cidr
  name       = "${local.name}-public"
}

resource "random_pet" "network_name" {
  prefix = var.name_prefix
  length = 2
  keepers = {
    stack = data.snapcd_stack.default.name
  }
}

resource "random_uuid" "nat_gateway_id" {
  depends_on = [time_sleep.network_propagation]
}

resource "random_uuid" "vpc_id" {}

resource "time_sleep" "network_propagation" {
  create_duration = "1s"
  depends_on      = [random_uuid.vpc_id, module.public_subnet, module.private_subnet]
}


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
  environment = local.settings.environment
  name        = "${var.name_prefix}-${local.environment}"
  settings    = jsondecode(data.local_file.environment.content)
}

# Environment settings, read from a checked-in file and decoded in locals.tf.
# Consumed by the database, cluster, and app sections below.
data "local_file" "environment" {
  filename = "${path.module}/config/environment.json"
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
}

resource "random_uuid" "nat_gateway_id" {
  depends_on = [time_sleep.network_propagation]
}

resource "random_uuid" "vpc_id" {}

resource "time_sleep" "network_propagation" {
  create_duration = "1s"
  depends_on      = [random_uuid.vpc_id, module.public_subnet, module.private_subnet]
}


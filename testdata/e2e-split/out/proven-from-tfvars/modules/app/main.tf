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
  }
}

provider "random" {}

# provider config → variable + local (structural carve).
provider "tls" {
  proxy {
    url = "https://${var.name_prefix}.${local.proxy_host}"
  }
}

# provider config → MODULE OUTPUT (cross-module): references module.idgen.id.
provider "tls" {
  alias = "by_module"
  proxy {
    url = "https://${var.module_idgen}.example"
  }
}

# provider config → RESOURCE (cross-module): references network.net_name.
provider "tls" {
  alias = "by_resource"
  proxy {
    url = "https://${var.random_pet_net_name}.example"
  }
}

variable "name_prefix" {
  type    = string
  default = "demo"
}

locals {
  common_length       = 3
  local_from_data_tag = var.random_pet_fp_tag
  local_from_module   = var.module_idgen
  local_from_resource = var.random_integer_net_id
  proxy_host          = "proxy.internal"
  tagged              = "${var.name_prefix}-instance"
}

# resource body → RESOURCE / MODULE OUTPUT (cross-module).
resource "random_pet" "instance" {
  length    = local.common_length
  separator = "-"
  keepers = {
    net    = var.random_integer_net_id # R
    token  = var.random_string_token   # R (other module)
    gen    = var.module_idgen          # M
    fp     = var.random_pet_fp_tag     # R (data module)
    tagged = local.tagged              # local (same module)
    lr     = local.local_from_resource # local → R
    lm     = local.local_from_module   # local → M
    ld     = local.local_from_data_tag # local → R
  }
}

# Default tls provider (var + local in config).
resource "tls_private_key" "cert" {
  algorithm = "ED25519"
}

resource "tls_private_key" "cert_by_module" {
  provider  = tls.by_module
  algorithm = "ED25519"
}

# Aliased providers whose configs reference R / M / D cross-module.
resource "tls_private_key" "cert_by_resource" {
  provider  = tls.by_resource
  algorithm = "ED25519"
}


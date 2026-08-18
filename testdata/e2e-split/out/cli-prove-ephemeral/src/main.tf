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

# ===========================================================================
# Full cross-reference matrix fixture.
#
# Producers (things referenced across a module boundary) come in two kinds:
#   R = resource output      (random_integer.net_id, random_pet.net_name, ...)
#   M = module-call output   (module.idgen.id)
#
# Consumers (things doing the referencing) come in five kinds:
#   resource body, module-call input, provider config, local value,
#   data-source argument.
#
# Every consumer×producer combination below is a real, applied, provable
# cross-module edge; the proof plans each carved module to zero-diff.
#
# Data sources are never producers across a boundary: a data source follows
# its consumers automatically, so every consumer reads a local copy. Here
# data.tls_public_key.pub has one consumer (fp_tag) and lands in `data` with
# it, reading signer's PEM through a same-module data-source argument;
# multi-module duplication is covered by the monolith fixture.
#
# Module placement:
#   network : net_id (R), net_name (R), idgen (M)
#   data    : token (R), signer (R, the PEM source), pub (D), fp_tag (R)
#   app     : consumers of R / M via every consumer kind
#   monolith: unmanaged catchall
# ===========================================================================

# --- providers -------------------------------------------------------------

provider "random" {}

# provider config → variable + local (structural carve).
provider "tls" {
  proxy {
    url = "https://${var.name_prefix}.${local.proxy_host}"
  }
}

# provider config → RESOURCE (cross-module): references network.net_name.
provider "tls" {
  alias = "by_resource"
  proxy {
    url = "https://${random_pet.net_name.id}.example"
  }
}

# provider config → MODULE OUTPUT (cross-module): references module.idgen.id.
provider "tls" {
  alias = "by_module"
  proxy {
    url = "https://${module.idgen.id}.example"
  }
}

# --- root variable + locals ------------------------------------------------

variable "name_prefix" {
  type    = string
  default = "demo"
}

locals {
  common_length = 3
  tagged        = "${var.name_prefix}-instance"
  proxy_host    = "proxy.internal"

  # local → RESOURCE / MODULE OUTPUT (cross-module, used in app).
  local_from_resource = random_integer.net_id.result
  local_from_module   = module.idgen.id
  local_from_data_tag = random_pet.fp_tag.id
}

# --- module: network -------------------------------------------------------

# @demono:move network
resource "random_integer" "net_id" {
  min = 1000
  max = 9999
}

# @demono:move network
resource "random_pet" "net_name" {
  length = local.common_length
  prefix = var.name_prefix
}

# Child module call (module-output producer). Its inputs exercise:
#   module input → RESOURCE (seed = net_id, same module)
#   module input → RESOURCE (tag = token, cross-module from `data`)
# @demono:move network
module "idgen" {
  source = "./modules/idgen"
  seed   = random_integer.net_id.result
  tag    = random_string.token.result
}

# --- module: data ----------------------------------------------------------

# @demono:move data
resource "random_string" "token" {
  length  = 12
  special = false
}

# A private key local to the `data` module; its PEM feeds the data source below
# (keeps data.pub's own input internal so the module graph stays acyclic).
# @demono:move data
resource "tls_private_key" "signer" {
  algorithm = "ED25519"
}

# data-source argument → RESOURCE (same module): reads signer's PEM. Never
# decorated — it follows its one consumer (fp_tag) into `data`.
data "tls_public_key" "pub" {
  private_key_pem = tls_private_key.signer.private_key_pem
}

# resource → DATA SOURCE (same module, keeps a data-arg consumer example) plus a
# plain resource producer for the token module.
# @demono:move data
resource "random_pet" "fp_tag" {
  length = local.common_length
  keepers = {
    fp = data.tls_public_key.pub.public_key_fingerprint_md5
  }
}

# --- module: app -----------------------------------------------------------

# resource body → RESOURCE / MODULE OUTPUT (cross-module).
# @demono:move app
resource "random_pet" "instance" {
  length    = local.common_length
  separator = "-"
  keepers = {
    net    = random_integer.net_id.result                            # R
    token  = random_string.token.result                             # R (other module)
    gen    = module.idgen.id                                        # M
    fp     = random_pet.fp_tag.id                                   # R (data module)
    tagged = local.tagged                                           # local (same module)
    lr     = local.local_from_resource                             # local → R
    lm     = local.local_from_module                               # local → M
    ld     = local.local_from_data_tag                             # local → R
  }
}

# Default tls provider (var + local in config).
# @demono:move app
resource "tls_private_key" "cert" {
  algorithm = "ED25519"
}

# Aliased providers whose configs reference R / M / D cross-module.
# @demono:move app
resource "tls_private_key" "cert_by_resource" {
  provider  = tls.by_resource
  algorithm = "ED25519"
}

# @demono:move app
resource "tls_private_key" "cert_by_module" {
  provider  = tls.by_module
  algorithm = "ED25519"
}

# --- catchall (undecorated -> remainder module) ----------------------------

resource "random_pet" "unmanaged_extra" {
  length = 2
}

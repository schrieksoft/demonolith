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
# Producers (things referenced across a module boundary) come in three kinds:
#   R = resource output      (random_integer.net_id, random_pet.net_name, ...)
#   M = module-call output   (module.idgen.id)
#   D = data-source result   (data.tls_public_key.pub.*)
#
# Consumers (things doing the referencing) come in five kinds:
#   resource body, module-call input, provider config, local value,
#   data-source argument.
#
# Every consumer×producer combination below is a real, applied, provable
# cross-module edge; the proof plans each carved module to zero-diff.
# (provider→data is omitted only because tls's proxy.url rejects the
# colon-delimited fingerprint; data-as-producer is covered by resource, local,
# and module-input consumers instead.)
#
# Module placement:
#   network : net_id (R), net_name (R), signer (R, a PEM source), idgen (M)
#   data    : token (R), pub (D, reads signer's PEM cross-module), fp_tag (R)
#   app     : consumers of R / M / D via every consumer kind
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

  # local → RESOURCE / MODULE OUTPUT / DATA SOURCE (all cross-module, used in app).
  local_from_resource = random_integer.net_id.result
  local_from_module   = module.idgen.id
  local_from_data     = data.tls_public_key.pub.public_key_fingerprint_md5
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
#   module input → DATA SOURCE (tag = data.pub, cross-module from `data`)
# @demono:move network
module "idgen" {
  source = "./modules/idgen"
  seed   = random_integer.net_id.result
  tag    = data.tls_public_key.pub.public_key_fingerprint_md5
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

# data-source argument → RESOURCE (same module): reads signer's PEM. The data
# source's RESULT is a cross-module producer (D) consumed by network (module
# input), app (resource body), and locals.
# @demono:move data
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

# resource body → RESOURCE / MODULE OUTPUT / DATA SOURCE (all cross-module).
# @demono:move app
resource "random_pet" "instance" {
  length    = local.common_length
  separator = "-"
  keepers = {
    net    = random_integer.net_id.result                            # R
    token  = random_string.token.result                             # R (other module)
    gen    = module.idgen.id                                        # M
    fp     = data.tls_public_key.pub.public_key_fingerprint_md5     # D
    tagged = local.tagged                                           # local (same module)
    lr     = local.local_from_resource                             # local → R
    lm     = local.local_from_module                               # local → M
    ld     = local.local_from_data                                 # local → D
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

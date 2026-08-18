provider "random" {}

# provider config → variable + local (structural carve).
provider "tls" {
  proxy {
    url = "https://${var.name_prefix}.${local.proxy_host}"
  }
}

variable "name_prefix" {
  type    = string
  default = "demo"
}

locals {
  common_length = 3
  proxy_host    = "proxy.internal"
}

# data-source argument → RESOURCE (same module): reads signer's PEM. Never
# decorated — it follows its one consumer (fp_tag) into `data`.
data "tls_public_key" "pub" {
  private_key_pem = tls_private_key.signer.private_key_pem
}

# resource → DATA SOURCE (same module, keeps a data-arg consumer example) plus a
# plain resource producer for the token module.
resource "random_pet" "fp_tag" {
  length = local.common_length
  keepers = {
    fp = data.tls_public_key.pub.public_key_fingerprint_md5
  }
}

resource "random_string" "token" {
  length  = 12
  special = false
}

# A private key local to the `data` module; its PEM feeds the data source below
# (keeps data.pub's own input internal so the module graph stays acyclic).
resource "tls_private_key" "signer" {
  algorithm = "ED25519"
}


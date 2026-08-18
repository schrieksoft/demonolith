locals {
  # Environment settings live in a checked-in config file; everything below
  # reads them through this one decoded object.
  settings = jsondecode(data.local_file.environment.content)

  environment = local.settings.environment
  name        = "${var.name_prefix}-${local.environment}"

  database_name = "${local.name}-${local.settings.database.name}"
  cluster_name  = "${local.name}-cluster"
  app_name      = "${local.name}-${local.settings.app.name}"

  # Values derived from resources; consumed on the far side of future seams.
  network_zone           = "${random_pet.network_name.id}.internal"
  deploy_key_fingerprint = data.tls_public_key.deploy_key.public_key_fingerprint_md5
}

locals {
  # Environment settings arrive as one JSON string variable (the content of
  # config/environment.json): content instead of a path, so nothing
  # path-relative crosses the split.
  settings = jsondecode(var.environment_json)

  environment = local.settings.environment
  name        = "${var.name_prefix}-${local.environment}"

  database_name = "${local.name}-${local.settings.database.name}"
  cluster_name  = "${local.name}-cluster"
  app_name      = "${local.name}-${local.settings.app.name}"

  # Values derived from resources; consumed on the far side of future seams.
  network_zone           = "${random_pet.network_name.id}.internal"
  deploy_key_fingerprint = data.tls_public_key.deploy_key.public_key_fingerprint_md5
}

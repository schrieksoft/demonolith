locals {
  environment = "e2e"
  name        = "${var.name_prefix}-${local.environment}"

  database_name = "${local.name}-orders"
  cluster_name  = "${local.name}-cluster"
  app_name      = "${local.name}-storefront"

  # Values derived from resources and data sources; consumed on the far side
  # of future seams.
  network_zone           = "${random_pet.network_name.id}.internal"
  deploy_key_fingerprint = data.tls_public_key.deploy_key.public_key_fingerprint_md5
  stack_id               = data.snapcd_stack.default.id
  runner_id              = data.snapcd_runner.default.id
}

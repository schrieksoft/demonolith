terraform {
  # The Snap CD State Store as the remote backend, with the seeded
  # default/default credentials. demonolith-e2e-1786982964507519935 is replaced by the test
  # with a unique per-run state file name.
  backend "http" {
    address        = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1786982964507519935-monolith"
    lock_address   = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1786982964507519935-monolith/lock"
    unlock_address = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-1786982964507519935-monolith/unlock"
    lock_method    = "POST"
    unlock_method  = "POST"
    username       = "default"
    password       = "default"
  }
}

terraform {
  backend "http" {
    lock_method    = "POST"
    unlock_method  = "POST"
    address        = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-flags-1786994862032382056-networking"
    lock_address   = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-flags-1786994862032382056-networking/lock"
    unlock_address = "http://localhost:5000/api/10000000-0000-0000-0000-000000000000/state/10000000-0000-0000-0000-000000000000/demonolith-e2e-flags-1786994862032382056-networking/unlock"
  }
}

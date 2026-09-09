variable "source_subdirectory_prefix" {
  type    = string
  default = ""
}

resource "snapcd_module" "source_root" {
  name                = "source"
  source_subdirectory = "source"
}

resource "snapcd_module" "shared_root" {
  name                = "shared"
  source_subdirectory = "${var.source_subdirectory_prefix}shared"
}

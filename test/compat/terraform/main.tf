# What test/compat/terraform.sh applies with the Terraform provider for
# Nexus: one resource of every kind Command Center serves, and a data source
# for each. step 2 changes attributes of most of them (the update).
terraform {
  required_providers {
    nexus = {
      source  = "datadrivers/nexus"
      version = "3.0.1"
    }
  }
}

# The address and the administrator come from NEXUS_URL, NEXUS_USERNAME and
# NEXUS_PASSWORD.
provider "nexus" {}

variable "step" {
  type    = number
  default = 1
}

locals {
  changed = var.step == 2
  # certs/azure.crt and apt-signing.asc are put beside this file by the script.
  cert = file("${path.module}/certs/azure.crt")
  idp_metadata = templatefile("${path.module}/idp-metadata.xml.tftpl", {
    certificate = replace(replace(replace(local.cert, "-----BEGIN CERTIFICATE-----", ""), "-----END CERTIFICATE-----", ""), "\n", "")
  })
}

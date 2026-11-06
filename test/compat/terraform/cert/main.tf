# The certificate of the Azure Blob endpoint the test stands in for (Azurite
# under the storage account's host name). The trust store resource and the
# SAML metadata carry it too. It is made here, so no key is in the repository.
terraform {
  required_providers {
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }
}

resource "tls_private_key" "azure" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "azure" {
  private_key_pem       = tls_private_key.azure.private_key_pem
  validity_period_hours = 24
  allowed_uses          = ["digital_signature", "server_auth"]
  dns_names             = ["devstoreaccount1.blob.core.windows.net"]
  subject {
    common_name = "devstoreaccount1.blob.core.windows.net"
  }
}

output "cert" {
  value = tls_self_signed_cert.azure.cert_pem
}

output "key" {
  value     = tls_private_key.azure.private_key_pem
  sensitive = true
}

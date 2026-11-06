# What Command Center leaves out of Nexus's API on purpose. Each resource
# must fail with the reason; test/compat/terraform.sh looks for it.
terraform {
  required_providers {
    nexus = {
      source  = "datadrivers/nexus"
      version = "3.0.1"
    }
  }
}

provider "nexus" {}

resource "nexus_script" "hello" {
  name    = "tf-hello"
  type    = "groovy"
  content = "log.info('hello')"
}

resource "nexus_privilege_script" "run" {
  name        = "tf-script-run"
  description = "run a script"
  script_name = "tf-hello"
  actions     = ["RUN"]
}

resource "nexus_capability" "outreach" {
  type    = "OutreachManagementCapability"
  enabled = false
}

resource "nexus_iqserver" "iq" {
  enabled             = false
  url                 = "http://iq.example.com:8070"
  authentication_type = "USER"
  username            = "iq"
  password            = "iq-secret"
}

resource "nexus_security_oidc" "sso" {
  client_id         = "commandcenter"
  client_secret     = "oidc-secret"
  authorization_url = "https://idp.example.com/auth"
  token_url         = "https://idp.example.com/token"
  jwks_url          = "https://idp.example.com/jwks"
  jws_algorithm     = "RS256"
  username_claim    = "preferred_username"
  groups_claim      = "groups"
}

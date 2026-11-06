# The settings that exist once: realms, anonymous access, user tokens (Nexus
# Pro), e-mail, SAML (Nexus Pro); and the certificates of the trust store.

resource "nexus_security_ssl_truststore" "azure" {
  pem = local.cert
}

# Both signature checks off would be refused: one of them is on in each step.
resource "nexus_security_saml" "saml" {
  idp_metadata                 = local.idp_metadata
  entity_id                    = local.changed ? "https://cc.example.com/saml" : null
  username_attribute           = local.changed ? "uid" : "username"
  first_name_attribute         = "firstName"
  last_name_attribute          = "lastName"
  email_attribute              = local.changed ? "mail" : "email"
  groups_attribute             = "groups"
  validate_response_signature  = true
  validate_assertion_signature = !local.changed
}

# Command Center's one directory, an OpenLDAP beside the server. Step 1 finds
# the engineers among the direct children of ou=people, step 2 everyone below
# it (the script signs in to see that). A group of the user's description
# attribute gives the role of its name, but never the administrators' role.
# A group_type of static would be refused: groups are read from the user.
resource "nexus_security_ldap" "directory" {
  name                           = "tf-ldap"
  protocol                       = "LDAP"
  host                           = "ldap"
  port                           = 1389
  search_base                    = "dc=example,dc=org"
  auth_schema                    = "SIMPLE"
  auth_username                  = "cn=admin,dc=example,dc=org"
  auth_password                  = "ldap-admin-secret"
  connection_timeout_seconds     = local.changed ? 120 : 30 # longer than 60 s waits 60 s here; it reads back as written
  connection_retry_delay_seconds = local.changed ? 60 : 300
  max_incident_count             = local.changed ? 5 : 3
  user_base_dn                   = "ou=people"
  user_subtree                   = local.changed
  user_object_class              = "inetOrgPerson"
  user_ldap_filter               = local.changed ? null : "(employeeType=engineer)"
  user_id_attribute              = "uid"
  user_real_name_attribute       = "cn"
  user_email_address_attribute   = "mail"
  ldap_groups_as_roles           = true
  group_type                     = local.changed ? "static" : "dynamic"
  group_base_dn                  = local.changed ? "ou=groups" : null
  group_object_class             = local.changed ? "groupOfNames" : null
  group_id_attribute             = local.changed ? "cn" : null
  group_member_attribute         = local.changed ? "member" : null
  group_member_format            = local.changed ? "$${dn}" : null
  user_member_of_attribute       = "description"
}

resource "nexus_security_ldap_order" "order" {
  order = [nexus_security_ldap.directory.name]
}

# The local realm stays first, so the administrator can sign in throughout.
resource "nexus_security_realms" "active" {
  active     = local.changed ? ["NexusAuthenticatingRealm", "LdapRealm", "NuGetApiKey", "SamlRealm"] : ["NexusAuthenticatingRealm", "LdapRealm", "SamlRealm", "DockerToken", "NpmToken"]
  depends_on = [nexus_security_saml.saml, nexus_security_ldap.directory]
}

resource "nexus_security_anonymous" "anonymous" {
  enabled    = !local.changed
  user_id    = "anonymous"
  realm_name = "NexusAuthorizingRealm"
}

resource "nexus_security_user_token" "tokens" {
  enabled            = true
  protect_content    = false
  expiration_enabled = local.changed
  expiration_days    = local.changed ? 90 : 30
}

# With TLS the server's identity is always checked (false is refused) and the
# trust store always used, whatever its switch says: step 2 leaves it at the
# provider's default.
resource "nexus_mail_config" "smtp" {
  enabled                           = !local.changed
  host                              = local.changed ? "mail.example.org" : "smtp.example.com"
  port                              = local.changed ? 465 : 587
  from_address                      = "cc@example.com"
  username                          = "mailer"
  password                          = local.changed ? "mail-secret-2" : "mail-secret-1"
  start_tls_enabled                 = !local.changed
  start_tls_required                = !local.changed
  ssl_on_connect_enabled            = local.changed
  ssl_server_identity_check_enabled = true
  nexus_trust_store_enabled         = !local.changed
}

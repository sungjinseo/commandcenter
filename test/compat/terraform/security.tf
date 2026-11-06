# Content selectors, privileges of every type, roles and users.

resource "nexus_security_content_selector" "docs" {
  name        = "tf-docs"
  description = local.changed ? "docs and guides" : "docs only"
  expression  = local.changed ? "format == \"raw\" and (path =^ \"/docs/\" or path =^ \"/guides/\")" : "format == \"raw\" and path =^ \"/docs/\""
}

resource "nexus_privilege_application" "users" {
  name        = "tf-app-users"
  description = "see the users"
  domain      = "users"
  actions     = local.changed ? ["READ", "EDIT"] : ["READ"]
}

resource "nexus_privilege_repository_view" "raw" {
  name        = "tf-view-raw"
  description = "use the raw repository"
  format      = "raw"
  repository  = nexus_repository_raw_hosted.h.name
  actions     = local.changed ? ["BROWSE", "READ", "EDIT", "ADD", "DELETE"] : ["BROWSE", "READ"]
}

# ADD on one repository is refused (it creates repositories: grant it on a
# format or on "*"), so the actions here have none.
resource "nexus_privilege_repository_admin" "raw" {
  name        = "tf-admin-raw"
  description = "manage the raw repository"
  format      = "raw"
  repository  = nexus_repository_raw_hosted.h.name
  actions     = local.changed ? ["BROWSE", "READ", "EDIT", "DELETE"] : ["READ", "EDIT"]
}

resource "nexus_privilege_repository_content_selector" "docs" {
  name             = "tf-selector-docs"
  description      = "the docs of the raw repository"
  format           = "raw"
  repository       = nexus_repository_raw_hosted.h.name
  content_selector = nexus_security_content_selector.docs.name
  actions          = local.changed ? ["READ", "BROWSE", "EDIT"] : ["READ", "BROWSE"]
}

resource "nexus_privilege_wildcard" "raw" {
  name        = "tf-wildcard"
  description = "every raw repository"
  pattern     = local.changed ? "nexus:repository-view:raw:*:browse,read" : "nexus:repository-view:raw:*:read"
}

resource "nexus_security_role" "docs" {
  roleid      = "tf-docs"
  name        = local.changed ? "Docs people" : "Docs readers"
  description = "made by terraform"
  privileges = concat([
    nexus_privilege_application.users.name,
    nexus_privilege_repository_view.raw.name,
    nexus_privilege_repository_admin.raw.name,
    nexus_privilege_repository_content_selector.docs.name,
  ], local.changed ? [] : [nexus_privilege_wildcard.raw.name])
  roles = []
}

# A built-in privilege by its Nexus name, and a contained role.
resource "nexus_security_role" "parent" {
  roleid      = "tf-parent"
  name        = "Parent"
  description = "contains the docs role"
  privileges  = ["nx-repository-view-raw-tf-raw-hosted-read"]
  roles       = local.changed ? [] : [nexus_security_role.docs.roleid]
  depends_on  = [nexus_repository_raw_hosted.h]
}

resource "nexus_security_user" "alice" {
  userid    = "tf-alice"
  firstname = local.changed ? "Alicia" : "Alice"
  lastname  = "Example"
  email     = local.changed ? "alicia@example.com" : "alice@example.com"
  password  = local.changed ? "alice-secret-2" : "alice-secret-1"
  roles     = local.changed ? [nexus_security_role.docs.roleid, nexus_security_role.parent.roleid] : [nexus_security_role.docs.roleid]
  status    = local.changed ? "disabled" : "active"
}

# The write-only password: sent when its version changes, never in the state.
resource "nexus_security_user" "bob" {
  userid              = "tf-bob"
  firstname           = "Bob"
  lastname            = "Example"
  email               = "bob@example.com"
  password_wo         = local.changed ? "bob-secret-2" : "bob-secret-1"
  password_wo_version = local.changed ? 2 : 1
  roles               = [nexus_security_role.docs.roleid]
}

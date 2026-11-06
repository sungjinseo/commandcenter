# A data source for everything the resources made. A value that reads back
# wrong fails its postcondition, and with it the apply.

data "nexus_repository_maven_hosted" "h" {
  name = nexus_repository_maven_hosted.h.name
}

data "nexus_repository_raw_hosted" "h" {
  name = nexus_repository_raw_hosted.h.name
}

data "nexus_repository_npm_hosted" "h" {
  name = nexus_repository_npm_hosted.h.name
}

data "nexus_repository_pypi_hosted" "h" {
  name = nexus_repository_pypi_hosted.h.name
}

data "nexus_repository_nuget_hosted" "h" {
  name = nexus_repository_nuget_hosted.h.name
}

data "nexus_repository_rubygems_hosted" "h" {
  name = nexus_repository_rubygems_hosted.h.name
}

data "nexus_repository_helm_hosted" "h" {
  name = nexus_repository_helm_hosted.h.name
}

data "nexus_repository_cargo_hosted" "h" {
  name = nexus_repository_cargo_hosted.h.name
}

data "nexus_repository_r_hosted" "h" {
  name = nexus_repository_r_hosted.h.name
}

data "nexus_repository_gitlfs_hosted" "h" {
  name = nexus_repository_gitlfs_hosted.h.name
}

data "nexus_repository_yum_hosted" "h" {
  name = nexus_repository_yum_hosted.h.name
}

data "nexus_repository_apt_hosted" "h" {
  name = nexus_repository_apt_hosted.h.name
}

data "nexus_repository_docker_hosted" "h" {
  name = nexus_repository_docker_hosted.h.name
}

data "nexus_repository_maven_proxy" "p" {
  name = nexus_repository_maven_proxy.p.name
}

data "nexus_repository_npm_proxy" "p" {
  name = nexus_repository_npm_proxy.p.name
}

data "nexus_repository_pypi_proxy" "p" {
  name = nexus_repository_pypi_proxy.p.name
}

data "nexus_repository_rubygems_proxy" "p" {
  name = nexus_repository_rubygems_proxy.p.name
}

data "nexus_repository_helm_proxy" "p" {
  name = nexus_repository_helm_proxy.p.name
}

data "nexus_repository_go_proxy" "p" {
  name = nexus_repository_go_proxy.p.name
}

data "nexus_repository_cargo_proxy" "p" {
  name = nexus_repository_cargo_proxy.p.name
}

data "nexus_repository_r_proxy" "p" {
  name = nexus_repository_r_proxy.p.name
}

data "nexus_repository_raw_proxy" "p" {
  name = nexus_repository_raw_proxy.p.name
}

data "nexus_repository_conan_proxy" "p" {
  name = nexus_repository_conan_proxy.p.name
}

data "nexus_repository_conda_proxy" "p" {
  name = nexus_repository_conda_proxy.p.name
}

data "nexus_repository_cocoapods_proxy" "p" {
  name = nexus_repository_cocoapods_proxy.p.name
}

data "nexus_repository_p2_proxy" "p" {
  name = nexus_repository_p2_proxy.p.name
}

data "nexus_repository_yum_proxy" "p" {
  name = nexus_repository_yum_proxy.p.name
}

data "nexus_repository_nuget_proxy" "p" {
  name = nexus_repository_nuget_proxy.p.name
}

data "nexus_repository_apt_proxy" "p" {
  name = nexus_repository_apt_proxy.p.name
}

data "nexus_repository_docker_proxy" "p" {
  name = nexus_repository_docker_proxy.p.name
}

data "nexus_repository_maven_group" "g" {
  name = nexus_repository_maven_group.g.name
}

data "nexus_repository_pypi_group" "g" {
  name = nexus_repository_pypi_group.g.name
}

data "nexus_repository_nuget_group" "g" {
  name = nexus_repository_nuget_group.g.name
}

data "nexus_repository_rubygems_group" "g" {
  name = nexus_repository_rubygems_group.g.name
}

data "nexus_repository_cargo_group" "g" {
  name = nexus_repository_cargo_group.g.name
}

data "nexus_repository_r_group" "g" {
  name = nexus_repository_r_group.g.name
}

data "nexus_repository_raw_group" "g" {
  name = nexus_repository_raw_group.g.name
}

data "nexus_repository_yum_group" "g" {
  name = nexus_repository_yum_group.g.name
}

data "nexus_repository_go_group" "g" {
  name = nexus_repository_go_group.g.name
}

data "nexus_repository_npm_group" "g" {
  name = nexus_repository_npm_group.g.name
}

data "nexus_repository_docker_group" "g" {
  name = nexus_repository_docker_group.g.name
}

data "nexus_repository_list" "all" {
  depends_on = [nexus_repository_docker_group.g, nexus_repository_raw_hosted.stored]
  lifecycle {
    postcondition {
      # The six it waits for: the others of the first apply may still be on their way.
      condition     = length([for r in self.items : r if startswith(r.name, "tf-")]) >= 6
      error_message = "The repository list does not hold the repositories this data source waits for."
    }
  }
}

data "nexus_blobstore_file" "a" {
  name = nexus_blobstore_file.a.name
}

data "nexus_blobstore_group" "g" {
  name = nexus_blobstore_group.g.name
  lifecycle {
    postcondition {
      condition     = self.fill_policy == nexus_blobstore_group.g.fill_policy && self.members == nexus_blobstore_group.g.members && try(length(self.soft_quota), 0) == length(nexus_blobstore_group.g.soft_quota)
      error_message = "The blob store group reads back with another fill policy, other members or another soft quota."
    }
  }
}

data "nexus_blobstore_s3" "s3" {
  name = nexus_blobstore_s3.s3.name
  lifecycle {
    postcondition {
      condition     = self.bucket_configuration[0].bucket[0].name == "tf-bucket" && self.bucket_configuration[0].bucket[0].expiration == -1
      error_message = "The S3 blob store reads back with another bucket or expiration."
    }
  }
}

data "nexus_blobstore_azure" "azure" {
  name = nexus_blobstore_azure.azure.name
}

data "nexus_blobstore_list" "all" {
  depends_on = [nexus_blobstore_group.g, nexus_blobstore_s3.s3, nexus_blobstore_azure.azure]
  lifecycle {
    postcondition {
      condition = toset([for b in self.items : "${b.name}:${b.type}"]) == toset([
        "default:File", "tf-file-a:File", "tf-file-b:File", "tf-group:Group", "tf-s3:S3", "tf-azure:Azure Cloud Storage",
      ])
      error_message = "The blob store list is not the default store and the five of this test."
    }
  }
}

data "nexus_routing_rule" "internal" {
  name = nexus_routing_rule.internal.name
}

data "nexus_security_content_selector" "docs" {
  name = nexus_security_content_selector.docs.name
}

data "nexus_privilege_application" "users" {
  name = nexus_privilege_application.users.name
}

data "nexus_privilege_repository_view" "raw" {
  name = nexus_privilege_repository_view.raw.name
}

data "nexus_privilege_repository_admin" "raw" {
  name = nexus_privilege_repository_admin.raw.name
}

data "nexus_privilege_repository_content_selector" "docs" {
  name = nexus_privilege_repository_content_selector.docs.name
  lifecycle {
    postcondition {
      condition     = self.content_selector == "tf-docs" && self.repository == "tf-raw-hosted"
      error_message = "The content selector privilege reads back with another selector or repository."
    }
  }
}

data "nexus_privilege_wildcard" "raw" {
  name = nexus_privilege_wildcard.raw.name
}

# The built-in administrator role, under its Nexus name.
data "nexus_security_role" "admin" {
  roleid = "nx-admin"
  lifecycle {
    postcondition {
      condition     = contains(self.privileges, "nx-all")
      error_message = "nx-admin does not hold nx-all."
    }
  }
}

data "nexus_security_role" "docs" {
  roleid = nexus_security_role.docs.roleid
}

data "nexus_security_user" "alice" {
  userid = nexus_security_user.alice.userid
  lifecycle {
    postcondition {
      condition     = self.email == nexus_security_user.alice.email && contains(self.roles, "tf-docs")
      error_message = "The user reads back with another address or without her role."
    }
  }
}

# Provider 3.0.1 reads the list and returns none of it (flattenSecurityLDAP
# ends in "return nil"), whatever the server answers: nothing to check here.
data "nexus_security_ldap" "all" {
  depends_on = [nexus_security_ldap.directory]
}

data "nexus_security_realms" "active" {
  depends_on = [nexus_security_realms.active]
  lifecycle {
    postcondition {
      condition     = join(" ", [for r in self.active : r.id]) == join(" ", nexus_security_realms.active.active)
      error_message = "The active realms do not read back in the order they were set."
    }
  }
}

data "nexus_security_anonymous" "anonymous" {
  depends_on = [nexus_security_anonymous.anonymous]
}

data "nexus_security_user_token" "tokens" {
  depends_on = [nexus_security_user_token.tokens]
}

data "nexus_security_saml" "saml" {
  depends_on = [nexus_security_saml.saml]
  lifecycle {
    postcondition {
      condition     = self.idp_metadata == local.idp_metadata
      error_message = "The identity provider metadata does not read back as written."
    }
  }
}

data "nexus_mail_config" "smtp" {
  depends_on = [nexus_mail_config.smtp]
}

data "nexus_security_ssl_truststore" "all" {
  depends_on = [nexus_security_ssl_truststore.azure]
  lifecycle {
    postcondition {
      condition     = contains([for c in self.certificates : c.subject_common_name], "devstoreaccount1.blob.core.windows.net")
      error_message = "The trust store does not list the certificate that was added."
    }
  }
}

# The certificate a host presents: the Azurite beside the server.
data "nexus_security_ssl" "azure" {
  host = "devstoreaccount1.blob.core.windows.net"
  port = 443
  lifecycle {
    postcondition {
      condition     = self.fingerprint == nexus_security_ssl_truststore.azure.fingerprint
      error_message = "The retrieved certificate is not the one in the trust store."
    }
  }
}

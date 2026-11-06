# Hosted repositories: every format the provider has a resource for.

resource "nexus_repository_maven_hosted" "h" {
  name   = "tf-maven-hosted"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = local.changed ? "ALLOW" : "ALLOW_ONCE"
  }
  maven {
    version_policy      = local.changed ? "MIXED" : "RELEASE"
    layout_policy       = local.changed ? "PERMISSIVE" : "STRICT"
    content_disposition = local.changed ? "ATTACHMENT" : "INLINE"
  }
}

# The write policy, the online switch and the content check change; a cleanup
# policy is added in step 2 (the proxy below starts with one and drops it).
resource "nexus_repository_raw_hosted" "h" {
  name   = "tf-raw-hosted"
  online = !local.changed
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = !local.changed
    write_policy                   = local.changed ? "DENY" : "ALLOW"
  }
  dynamic "cleanup" {
    for_each = local.changed ? [1] : []
    content {
      policy_names = ["tf-cleanup"]
    }
  }
}

resource "nexus_repository_npm_hosted" "h" {
  name   = "tf-npm-hosted"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = local.changed ? "ALLOW_ONCE" : "ALLOW"
  }
}

# proprietary_components is Nexus Firewall's: kept and read back, no more.
resource "nexus_repository_pypi_hosted" "h" {
  name   = "tf-pypi-hosted"
  online = true
  component {
    proprietary_components = !local.changed
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = "ALLOW"
  }
}

resource "nexus_repository_nuget_hosted" "h" {
  name   = "tf-nuget-hosted"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = local.changed ? "ALLOW_ONCE" : "ALLOW"
  }
}

resource "nexus_repository_rubygems_hosted" "h" {
  name   = "tf-rubygems-hosted"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = local.changed ? "ALLOW_ONCE" : "ALLOW"
  }
}

resource "nexus_repository_helm_hosted" "h" {
  name   = "tf-helm-hosted"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = local.changed ? "ALLOW_ONCE" : "ALLOW"
  }
}

resource "nexus_repository_cargo_hosted" "h" {
  name   = "tf-cargo-hosted"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = local.changed ? "ALLOW_ONCE" : "ALLOW"
  }
}

resource "nexus_repository_r_hosted" "h" {
  name   = "tf-r-hosted"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = local.changed ? "ALLOW_ONCE" : "ALLOW"
  }
}

resource "nexus_repository_gitlfs_hosted" "h" {
  name   = "tf-gitlfs-hosted"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = local.changed ? "ALLOW_ONCE" : "ALLOW"
  }
}

# PERMISSIVE allows at depth 0 what STRICT does, so it reads back; at depth 1
# (step 2) only STRICT is accepted.
resource "nexus_repository_yum_hosted" "h" {
  name           = "tf-yum-hosted"
  online         = true
  deploy_policy  = local.changed ? "STRICT" : "PERMISSIVE"
  repodata_depth = local.changed ? 1 : 0
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = "ALLOW"
  }
}

resource "nexus_repository_apt_hosted" "h" {
  name         = "tf-apt-hosted"
  online       = true
  distribution = local.changed ? "trixie" : "bookworm"
  signing {
    keypair    = file("${path.module}/apt-signing.asc")
    passphrase = ""
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = "ALLOW"
  }
}

# force_basic_auth is the repository's anonymous-read override: true asks for
# credentials whatever the global setting says. There are no connector
# ports; path_based_routing reads back as written.
resource "nexus_repository_docker_hosted" "h" {
  name   = "tf-docker-hosted"
  online = true
  docker {
    force_basic_auth   = !local.changed
    v1_enabled         = false
    path_based_routing = true
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
    write_policy                   = "ALLOW"
  }
}

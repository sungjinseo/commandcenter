# Group repositories: every format the provider has a resource for.

resource "nexus_repository_maven_group" "g" {
  name   = "tf-maven-group"
  online = true
  group {
    member_names = local.changed ? [nexus_repository_maven_proxy.p.name, nexus_repository_maven_hosted.h.name] : [nexus_repository_maven_hosted.h.name, nexus_repository_maven_proxy.p.name]
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

resource "nexus_repository_pypi_group" "g" {
  name   = "tf-pypi-group"
  online = true
  group {
    member_names = local.changed ? [nexus_repository_pypi_proxy.p.name, nexus_repository_pypi_hosted.h.name] : [nexus_repository_pypi_hosted.h.name, nexus_repository_pypi_proxy.p.name]
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

resource "nexus_repository_nuget_group" "g" {
  name   = "tf-nuget-group"
  online = true
  group {
    member_names = local.changed ? [nexus_repository_nuget_proxy.p.name, nexus_repository_nuget_hosted.h.name] : [nexus_repository_nuget_hosted.h.name, nexus_repository_nuget_proxy.p.name]
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

resource "nexus_repository_rubygems_group" "g" {
  name   = "tf-rubygems-group"
  online = true
  group {
    member_names = local.changed ? [nexus_repository_rubygems_proxy.p.name, nexus_repository_rubygems_hosted.h.name] : [nexus_repository_rubygems_hosted.h.name, nexus_repository_rubygems_proxy.p.name]
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

resource "nexus_repository_cargo_group" "g" {
  name   = "tf-cargo-group"
  online = true
  group {
    member_names = local.changed ? [nexus_repository_cargo_proxy.p.name, nexus_repository_cargo_hosted.h.name] : [nexus_repository_cargo_hosted.h.name, nexus_repository_cargo_proxy.p.name]
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

resource "nexus_repository_r_group" "g" {
  name   = "tf-r-group"
  online = true
  group {
    member_names = local.changed ? [nexus_repository_r_proxy.p.name, nexus_repository_r_hosted.h.name] : [nexus_repository_r_hosted.h.name, nexus_repository_r_proxy.p.name]
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

# A group stores nothing here, but the blob store it names reads back.
resource "nexus_repository_raw_group" "g" {
  name   = "tf-raw-group"
  online = true
  group {
    member_names = local.changed ? [nexus_repository_raw_proxy.p.name, nexus_repository_raw_hosted.h.name] : [nexus_repository_raw_hosted.h.name, nexus_repository_raw_proxy.p.name]
  }
  storage {
    blob_store_name                = nexus_blobstore_file.a.name
    strict_content_type_validation = true
  }
}

resource "nexus_repository_yum_group" "g" {
  name   = "tf-yum-group"
  online = true
  group {
    member_names = local.changed ? [nexus_repository_yum_proxy.p.name, nexus_repository_yum_hosted.h.name] : [nexus_repository_yum_hosted.h.name, nexus_repository_yum_proxy.p.name]
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

resource "nexus_repository_go_group" "g" {
  name   = "tf-go-group"
  online = !local.changed
  group {
    member_names = [nexus_repository_go_proxy.p.name]
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

# The member order and the member that takes uploads change.
resource "nexus_repository_npm_group" "g" {
  name   = "tf-npm-group"
  online = true
  group {
    member_names    = local.changed ? [nexus_repository_npm_proxy.p.name, nexus_repository_npm_hosted.h.name] : [nexus_repository_npm_hosted.h.name, nexus_repository_npm_proxy.p.name]
    writable_member = local.changed ? null : nexus_repository_npm_hosted.h.name
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = !local.changed
  }
}

resource "nexus_repository_docker_group" "g" {
  name   = "tf-docker-group"
  online = true
  docker {
    force_basic_auth = local.changed
    v1_enabled       = false
  }
  group {
    member_names    = [nexus_repository_docker_hosted.h.name, nexus_repository_docker_proxy.p.name]
    writable_member = nexus_repository_docker_hosted.h.name
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
}

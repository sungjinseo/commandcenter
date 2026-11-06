# Proxy repositories: every format the provider has a resource for. The
# remotes are never asked: nothing is fetched through these repositories.

resource "nexus_routing_rule" "internal" {
  name        = "tf-internal"
  description = local.changed ? "only the public groups" : "no internal groups from the remote"
  mode        = local.changed ? "ALLOW" : "BLOCK"
  matchers    = local.changed ? ["^/org/public/.*"] : ["^/com/example/.*", "^/org/example/.*"]
}

# Everything a proxy can carry in step 1 (credentials, connection settings,
# a routing rule, a cleanup policy); step 2 takes the credentials, the rule
# and the policy away again, which a PUT without them must do.
resource "nexus_repository_maven_proxy" "p" {
  name         = "tf-maven-proxy"
  online       = true
  routing_rule = local.changed ? null : nexus_routing_rule.internal.name
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://maven.example.com/maven2" # no trailing slash: it reads back as written
    content_max_age  = local.changed ? 60 : -1
    metadata_max_age = local.changed ? 0 : 30
  }
  negative_cache {
    enabled = !local.changed
    ttl     = local.changed ? 1440 : 5
  }
  http_client {
    blocked    = local.changed
    auto_block = !local.changed
    dynamic "authentication" {
      for_each = local.changed ? [] : [1]
      content {
        type       = "username"
        username   = "remote-user"
        password   = "remote-secret"
        preemptive = true
      }
    }
    connection {
      retries                   = 2
      timeout                   = local.changed ? 120 : 60
      user_agent_suffix         = "terraform"
      enable_circular_redirects = true
      enable_cookies            = true
      use_trust_store           = true
    }
  }
  dynamic "cleanup" {
    for_each = local.changed ? [] : [1]
    content {
      policy_names = ["tf-cleanup"]
    }
  }
  maven {
    version_policy = local.changed ? "MIXED" : "RELEASE"
    layout_policy  = local.changed ? "PERMISSIVE" : "STRICT"
  }
}

resource "nexus_repository_npm_proxy" "p" {
  name   = "tf-npm-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://npm.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_pypi_proxy" "p" {
  name   = "tf-pypi-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://pypi.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_rubygems_proxy" "p" {
  name   = "tf-rubygems-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://rubygems.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_helm_proxy" "p" {
  name   = "tf-helm-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://helm.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_go_proxy" "p" {
  name   = "tf-go-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://go.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_cargo_proxy" "p" {
  name   = "tf-cargo-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://cargo.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_r_proxy" "p" {
  name   = "tf-r-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://r.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_raw_proxy" "p" {
  name   = "tf-raw-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://raw.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_conan_proxy" "p" {
  name   = "tf-conan-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://conan.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_conda_proxy" "p" {
  name   = "tf-conda-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://conda.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_cocoapods_proxy" "p" {
  name   = "tf-cocoapods-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://cocoapods.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_p2_proxy" "p" {
  name   = "tf-p2-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://p2.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_yum_proxy" "p" {
  name   = "tf-yum-proxy"
  online = true
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://yum.example.com/"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

# The query cache age is kept and read back; queries follow metadata_max_age.
resource "nexus_repository_nuget_proxy" "p" {
  name                     = "tf-nuget-proxy"
  online                   = true
  nuget_version            = "V3"
  query_cache_item_max_age = local.changed ? 3600 : 1440
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://nuget.example.com/v3/index.json"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

resource "nexus_repository_apt_proxy" "p" {
  name         = "tf-apt-proxy"
  online       = true
  distribution = local.changed ? "trixie" : "bookworm"
  flat         = local.changed
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://apt.example.com/debian"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

# There is no docker search, so the index type is kept and read back.
resource "nexus_repository_docker_proxy" "p" {
  name   = "tf-docker-proxy"
  online = true
  docker {
    force_basic_auth = false
    v1_enabled       = false
  }
  docker_proxy {
    index_type = local.changed ? "REGISTRY" : "HUB"
  }
  storage {
    blob_store_name                = "default"
    strict_content_type_validation = true
  }
  proxy {
    remote_url       = "https://registry.example.com"
    content_max_age  = local.changed ? 720 : 1440
    metadata_max_age = local.changed ? 60 : 1440
  }
  negative_cache {
    enabled = true
    ttl     = 1440
  }
  http_client {
    blocked    = false
    auto_block = true
  }
}

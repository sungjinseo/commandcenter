# Blob stores of every type the provider has: file, group (Nexus Pro), S3 (a
# SeaweedFS beside the server) and Azure (Nexus Pro; an Azurite that answers
# under the storage account's host name), each with a repository on it.

resource "nexus_blobstore_file" "a" {
  name = "tf-file-a"
  path = "tf-file-a"
  dynamic "soft_quota" {
    for_each = local.changed ? [1] : []
    content {
      limit = 2000000000
      type  = "spaceUsedQuota"
    }
  }
}

resource "nexus_blobstore_file" "b" {
  name = "tf-file-b"
  path = "tf-file-b"
  dynamic "soft_quota" {
    for_each = local.changed ? [] : [1]
    content {
      limit = 1000000000
      type  = "spaceRemainingQuota"
    }
  }
}

resource "nexus_blobstore_group" "g" {
  name        = "tf-group"
  fill_policy = local.changed ? "writeToFirst" : "roundRobin"
  members     = local.changed ? [nexus_blobstore_file.b.name] : [nexus_blobstore_file.a.name, nexus_blobstore_file.b.name]
  dynamic "soft_quota" {
    for_each = local.changed ? [] : [1]
    content {
      limit = 4000000000
      type  = "spaceUsedQuota"
    }
  }
}

# expiration only reads back: deleted blobs are removed by the blobstore.compact
# task, not by a bucket lifecycle rule.
resource "nexus_blobstore_s3" "s3" {
  name = "tf-s3"
  bucket_configuration {
    bucket {
      name       = "tf-bucket"
      region     = "us-east-1"
      prefix     = "cc"
      expiration = -1
    }
    bucket_security {
      access_key_id     = "tfaccesskey"
      secret_access_key = "tfsecretkey0123456789"
    }
    advanced_bucket_connection {
      endpoint         = "http://s3:8333"
      force_path_style = true
      signer_type      = "DEFAULT"
    }
  }
  dynamic "soft_quota" {
    for_each = local.changed ? [1] : []
    content {
      limit = 5000000000
      type  = "spaceUsedQuota"
    }
  }
}

# The account and its key are Azurite's published development account.
resource "nexus_blobstore_azure" "azure" {
  name = "tf-azure"
  bucket_configuration {
    account_name   = "devstoreaccount1"
    container_name = "tf-container"
    authentication {
      authentication_method = "ACCOUNTKEY"
      account_key           = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
    }
  }
  dynamic "soft_quota" {
    for_each = local.changed ? [1] : []
    content {
      limit = 3000000000
      type  = "spaceUsedQuota"
    }
  }
}

resource "nexus_repository_raw_hosted" "stored" {
  for_each = {
    group = nexus_blobstore_group.g.name
    s3    = nexus_blobstore_s3.s3.name
    azure = nexus_blobstore_azure.azure.name
  }
  name   = "tf-raw-on-${each.key}"
  online = true
  storage {
    blob_store_name                = each.value
    strict_content_type_validation = false
    write_policy                   = local.changed ? "ALLOW_ONCE" : "ALLOW"
  }
}

package nexusapi

import (
	"net/http"

	"commandcenter/internal/openapi"
)

// The OpenAPI description of the compat routes (feature: OpenAPI document).
// Nexus serves Swagger 2.0 at /service/rest/swagger.json; this server
// answers that path with the OpenAPI 3.0 document of both APIs.

func (h *Handler) swagger(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(openapi.Document())
}

func init() { openapi.Register(apiDocs...) }

const (
	nx      = "nexus"
	nxAdmin = "admin"
)

var apiDocs = []openapi.Route{
	{Pattern: "GET /service/rest/swagger.json", Summary: "This document (OpenAPI 3.0, where Nexus serves Swagger 2.0)", Tag: nx},
	{Pattern: "GET " + Prefix + "/status", Summary: "Liveness: 200", Tag: nx},
	{Pattern: "GET " + Prefix + "/status/writable", Summary: "Readiness: 200 when the database answers and the node is not draining, its schema is compatible and its lock connection and listener are up; else 503", Tag: nx},
	{Pattern: "GET " + Prefix + "/status/check", Summary: "The health checks in Nexus's shape", Tag: nx, Auth: nxAdmin, Reply: "NexusChecks"},
	{Pattern: "GET " + Prefix + "/repositories", Summary: "Readable repositories", Tag: nx, Reply: "NexusRepositoryList"},
	{Pattern: "GET " + Prefix + "/repositories/{name}", Summary: "One repository", Tag: nx, Reply: "NexusRepository"},
	{Pattern: "GET " + Prefix + "/repositorySettings", Summary: "Every repository's settings (storage.strictContentTypeValidation, httpClient.blocked/autoBlock, raw.contentDisposition included)", Tag: nx, Auth: nxAdmin, Reply: "NexusRepositorySettingsList"},
	{Pattern: "GET " + Prefix + "/repositories/{format}/{type}/{name}", Summary: "One repository's settings", Tag: nx, Auth: nxAdmin, Reply: "NexusRepositorySettings"},
	{Pattern: "POST " + Prefix + "/repositories/{format}/{type}", Summary: "Creates a repository from a Nexus body", Tag: nx, Auth: nxAdmin, Request: "NexusRepositoryBody", Replies: map[int]string{201: "created", 400: "NexusValidation"}},
	{Pattern: "PUT " + Prefix + "/repositories/{format}/{type}/{name}", Summary: "Updates a repository from a Nexus body", Tag: nx, Auth: nxAdmin, Request: "NexusRepositoryBody", Replies: map[int]string{204: "updated", 400: "NexusValidation"}},
	{Pattern: "DELETE " + Prefix + "/repositories/{name}", Summary: "Deletes a repository", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted"}},
	{Pattern: "GET " + Prefix + "/components", Summary: "Components of a repository, paged", Tag: nx, Query: []string{"repository", "continuationToken"}, Reply: "NexusComponentPage"},
	{Pattern: "GET " + Prefix + "/components/{id}", Summary: "One component", Tag: nx, Reply: "NexusComponent"},
	{Pattern: "DELETE " + Prefix + "/components/{id}", Summary: "Deletes a component", Tag: nx, Auth: "a user with delete", Replies: map[int]string{204: "deleted"}},
	{Pattern: "POST " + Prefix + "/components", Summary: "Uploads a component: a multipart/form-data body with the format's fields (<format>.asset[N], <format>.asset[N].<field>, <format>.<field>; see upload-specs); each file goes through the format's own upload", Tag: nx, Auth: "a user with add", Query: []string{"repository"}, Replies: map[int]string{204: "uploaded", 400: "NexusValidation", 403: "Error", 404: "Error", 413: "Error"}},
	{Pattern: "GET " + Prefix + "/formats/upload-specs", Summary: "The upload specs of the formats the component upload takes: format, multipleUpload, componentFields, assetFields", Tag: nx, Reply: "NexusUploadSpecList"},
	{Pattern: "GET " + Prefix + "/formats/{format}/upload-specs", Summary: "One format's upload spec (Nexus format name, maven2 for Maven); 404 when the format has no API upload", Tag: nx, Reply: "NexusUploadSpec", Replies: map[int]string{404: "Error"}},
	{Pattern: "GET " + Prefix + "/assets", Summary: "Assets of a repository, paged", Tag: nx, Query: []string{"repository", "continuationToken"}, Reply: "NexusAssetPage"},
	{Pattern: "GET " + Prefix + "/assets/{id}", Summary: "One asset", Tag: nx, Reply: "NexusAsset"},
	{Pattern: "DELETE " + Prefix + "/assets/{id}", Summary: "Deletes a raw file (other formats: use components)", Tag: nx, Auth: "a user with delete", Replies: map[int]string{204: "deleted", 400: "Error"}},
	{Pattern: "GET " + Prefix + "/search", Summary: "Component search", Tag: nx, Query: []string{"q", "repository", "format", "group", "name", "version", "sort", "direction", "continuationToken"}, Reply: "NexusComponentPage"},
	{Pattern: "GET " + Prefix + "/search/assets", Summary: "Asset search", Tag: nx, Query: []string{"q", "repository", "format", "group", "name", "version", "sort", "direction", "continuationToken"}, Reply: "NexusAssetPage"},
	{Pattern: "GET " + Prefix + "/search/assets/download", Summary: "Redirects to the one matching asset", Tag: nx, Query: []string{"q", "repository", "format", "group", "name", "version", "sort", "direction"}, Replies: map[int]string{302: "redirect", 400: "Error", 404: "Error"}},
	{Pattern: "GET " + Prefix + "/security/users", Summary: "Stored users (userId: a case-insensitive prefix)", Tag: nx, Auth: nxAdmin, Query: []string{"userId"}, Reply: "NexusUserList"},
	{Pattern: "GET " + Prefix + "/security/roles", Summary: "Roles", Tag: nx, Auth: nxAdmin, Reply: "NexusRoleList"},
	{Pattern: "GET " + Prefix + "/blobstores", Summary: "Blob stores with softQuota", Tag: nx, Auth: nxAdmin, Reply: "NexusBlobStoreList"},
	{Pattern: "GET " + Prefix + "/blobstores/{name}/{sub}", Summary: "{sub} = quota-status: the soft quota state (isViolation, message, blobStoreName); other {sub} 404", Tag: nx, Auth: nxAdmin, Reply: "NexusQuotaStatus"},
	{Pattern: "POST " + Prefix + "/blobstores/file", Summary: "Creates a file blob store: name, path, softQuota {type, limit (bytes)}", Tag: nx, Auth: nxAdmin, Request: "NexusFileBlobStore", Replies: map[int]string{204: "created", 400: "NexusValidation"}},
	{Pattern: "PUT " + Prefix + "/blobstores/file/{name}", Summary: "Updates a file blob store's softQuota (a different path is 400: it is immutable)", Tag: nx, Auth: nxAdmin, Request: "NexusFileBlobStore", Replies: map[int]string{204: "updated", 400: "NexusValidation", 404: "Error"}},
	{Pattern: "GET " + Prefix + "/tasks", Summary: "Scheduled tasks", Tag: nx, Auth: nxAdmin, Reply: "NexusTaskPage"},
	{Pattern: "GET " + Prefix + "/routing-rules", Summary: "Routing rules", Tag: nx, Auth: nxAdmin, Reply: "NexusRoutingRuleList"},
	{Pattern: "POST " + Prefix + "/routing-rules", Summary: "Creates a routing rule", Tag: nx, Auth: nxAdmin, Request: "NexusRoutingRule", Replies: map[int]string{204: "created", 409: "Error"}},
	{Pattern: "GET " + Prefix + "/routing-rules/{name}", Summary: "One routing rule", Tag: nx, Auth: nxAdmin, Reply: "NexusRoutingRule"},
	{Pattern: "PUT " + Prefix + "/routing-rules/{name}", Summary: "Updates a routing rule", Tag: nx, Auth: nxAdmin, Request: "NexusRoutingRule", Replies: map[int]string{204: "updated"}},
	{Pattern: "DELETE " + Prefix + "/routing-rules/{name}", Summary: "Deletes a routing rule (400 while a repository uses it)", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted", 400: "Error", 404: "Error"}},
	{Pattern: "GET " + Prefix + "/security/content-selectors", Summary: "Content selectors", Tag: nx, Auth: nxAdmin, Reply: "NexusSelectorList"},
	{Pattern: "POST " + Prefix + "/security/content-selectors", Summary: "Creates a content selector", Tag: nx, Auth: nxAdmin, Request: "NexusSelector", Replies: map[int]string{204: "created"}},
	{Pattern: "GET " + Prefix + "/security/content-selectors/{name}", Summary: "One content selector", Tag: nx, Auth: nxAdmin, Reply: "NexusSelector"},
	{Pattern: "PUT " + Prefix + "/security/content-selectors/{name}", Summary: "Updates a content selector", Tag: nx, Auth: nxAdmin, Request: "NexusSelector", Replies: map[int]string{204: "updated"}},
	{Pattern: "DELETE " + Prefix + "/security/content-selectors/{name}", Summary: "Deletes a content selector", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted"}},
	{Pattern: "POST " + Prefix + "/security/privileges/repository-content-selector", Summary: "Creates a content-selector privilege (a role priv:<name>)", Tag: nx, Auth: nxAdmin, Request: "NexusSelectorPrivilege", Replies: map[int]string{201: "created"}},
	{Pattern: "PUT " + Prefix + "/security/privileges/repository-content-selector/{name}", Summary: "Updates a content-selector privilege", Tag: nx, Auth: nxAdmin, Request: "NexusSelectorPrivilege", Replies: map[int]string{204: "updated"}},
	{Pattern: "GET " + Prefix + "/security/ssl", Summary: "The trust store", Tag: nx, Auth: nxAdmin, Reply: "NexusCertificateList"},
	{Pattern: "POST " + Prefix + "/security/ssl/truststore", Summary: "Adds a PEM certificate", Tag: nx, Auth: nxAdmin, Request: "NexusCertificate", Replies: map[int]string{201: "created"}},
	{Pattern: "DELETE " + Prefix + "/security/ssl/truststore/{id}", Summary: "Removes a certificate", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "removed"}},
	{Pattern: "GET " + Prefix + "/security/user-tokens", Summary: "User token settings (enabled, protectContent, expirationEnabled, expirationDays)", Tag: nx, Auth: nxAdmin, Reply: "NexusUserTokenSettings"},
	{Pattern: "PUT " + Prefix + "/security/user-tokens", Summary: "Replaces the user token settings", Tag: nx, Auth: nxAdmin, Request: "NexusUserTokenSettings", Reply: "NexusUserTokenSettings", Replies: map[int]string{400: "NexusValidation"}},
	{Pattern: "POST " + Prefix + "/security/user-tokens", Summary: "Replaces the user token settings (POST form)", Tag: nx, Auth: nxAdmin, Request: "NexusUserTokenSettings", Reply: "NexusUserTokenSettings", Replies: map[int]string{400: "NexusValidation"}},
	{Pattern: "DELETE " + Prefix + "/security/user-tokens", Summary: "Invalidates every user token", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "invalidated"}},
	{Pattern: "GET " + Prefix + "/security/user-tokens/tokens", Summary: "User tokens, filtered and paged", Tag: nx, Auth: nxAdmin, Query: []string{"realm", "userId", "nameCode", "includeExpired", "skip", "limit"}, Reply: "NexusUserTokenList"},
	{Pattern: "GET " + Prefix + "/security/users/{userId}/{realm}/user-token", Summary: "One user's token", Tag: nx, Auth: nxAdmin, Reply: "NexusUserToken", Replies: map[int]string{404: "Error"}},
	{Pattern: "POST " + Prefix + "/security/users/{userId}/{realm}/user-token", Summary: "Creates or resets one user's token (returns the pass code)", Tag: nx, Auth: nxAdmin, Reply: "NexusUserToken"},
	{Pattern: "DELETE " + Prefix + "/security/users/{userId}/{realm}/user-token", Summary: "Deletes one user's token", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted", 404: "Error"}},
	{Pattern: "DELETE " + Prefix + "/security/users/{userId}/{realm}/user-token-reset", Summary: "Alias of the user-token DELETE", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted", 404: "Error"}},
	{Pattern: "GET " + Prefix + "/blobstores/group", Summary: "Blob store groups: name, members, fillPolicy, softQuota", Tag: nx, Auth: nxAdmin, Reply: "NexusBlobStoreGroupList"},
	{Pattern: "POST " + Prefix + "/blobstores/group", Summary: "Creates a blob store group", Tag: nx, Auth: nxAdmin, Request: "NexusBlobStoreGroup", Replies: map[int]string{204: "created", 400: "NexusValidation"}},
	{Pattern: "GET " + Prefix + "/blobstores/group/{name}", Summary: "One blob store group", Tag: nx, Auth: nxAdmin, Reply: "NexusBlobStoreGroup", Replies: map[int]string{404: "Error"}},
	{Pattern: "PUT " + Prefix + "/blobstores/group/{name}", Summary: "Replaces a group's members and fill policy (keeps read-only flags of kept members)", Tag: nx, Auth: nxAdmin, Request: "NexusBlobStoreGroup", Replies: map[int]string{204: "updated", 400: "NexusValidation"}},
	{Pattern: "GET " + Prefix + "/cleanup-policies", Summary: "Cleanup policies in Nexus's shape (criteriaLastBlobUpdated, criteriaLastDownloaded, criteriaReleaseType, criteriaAssetRegex, retain, sortBy)", Tag: nx, Auth: nxAdmin, Reply: "NexusCleanupPolicyList"},
	{Pattern: "POST " + Prefix + "/cleanup-policies", Summary: "Creates a cleanup policy", Tag: nx, Auth: nxAdmin, Request: "NexusCleanupPolicy", Reply: "NexusCleanupPolicy", Replies: map[int]string{201: "created", 400: "NexusValidation", 409: "Error"}},
	{Pattern: "GET " + Prefix + "/cleanup-policies/{name}", Summary: "One cleanup policy", Tag: nx, Auth: nxAdmin, Reply: "NexusCleanupPolicy", Replies: map[int]string{404: "Error"}},
	{Pattern: "PUT " + Prefix + "/cleanup-policies/{name}", Summary: "Replaces a cleanup policy (keeps namePattern and exclude)", Tag: nx, Auth: nxAdmin, Request: "NexusCleanupPolicy", Replies: map[int]string{204: "updated", 400: "NexusValidation", 404: "Error"}},
	{Pattern: "DELETE " + Prefix + "/cleanup-policies/{name}", Summary: "Deletes a cleanup policy no repository uses", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted", 404: "Error", 409: "Error"}},
	{Pattern: "POST " + Prefix + "/tasks", Summary: "Creates a task from Nexus's TaskTemplateXO (type, name, enabled, alertEmail, frequency, properties)", Tag: nx, Auth: nxAdmin, Request: "NexusTaskTemplate", Reply: "NexusTask", Replies: map[int]string{201: "created", 400: "NexusValidation"}},
	{Pattern: "GET " + Prefix + "/tasks/{id}", Summary: "One task", Tag: nx, Auth: nxAdmin, Reply: "NexusTask", Replies: map[int]string{404: "Error"}},
	{Pattern: "PUT " + Prefix + "/tasks/{id}", Summary: "Updates a task (absent fields kept, the type cannot change)", Tag: nx, Auth: nxAdmin, Request: "NexusTaskTemplate", Replies: map[int]string{204: "updated", 400: "NexusValidation", 404: "Error"}},
	{Pattern: "DELETE " + Prefix + "/tasks/{id}", Summary: "Deletes a task", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted", 404: "Error"}},
	{Pattern: "POST " + Prefix + "/tasks/{id}/run", Summary: "Runs a task now", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "started", 404: "Error", 409: "Error"}},
	{Pattern: "POST " + Prefix + "/tasks/{id}/stop", Summary: "Stops a running task", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "stopping", 404: "Error", 409: "Error"}},
	{Pattern: "GET /service/rest/atlas/system-information", Summary: "Status, configuration, runtime, time and blob store free space in Nexus's sections, without secrets", Tag: nx, Auth: nxAdmin, Reply: "NexusSystemInformation"},
	{Pattern: "GET " + Prefix + "/tags", Summary: "Tags (items, continuationToken)", Tag: nx, Auth: nxAdmin, Query: []string{"continuationToken"}, Reply: "NexusTagList"},
	{Pattern: "POST " + Prefix + "/tags", Summary: "Creates a tag", Tag: nx, Auth: nxAdmin, Request: "NexusTag", Reply: "NexusTag", Replies: map[int]string{400: "Error", 409: "Error"}},
	{Pattern: "GET " + Prefix + "/tags/{name}", Summary: "One tag", Tag: nx, Auth: nxAdmin, Reply: "NexusTag", Replies: map[int]string{404: "Error"}},
	{Pattern: "PUT " + Prefix + "/tags/{name}", Summary: "Replaces a tag's attributes", Tag: nx, Auth: nxAdmin, Request: "NexusTag", Reply: "NexusTag", Replies: map[int]string{400: "Error", 404: "Error"}},
	{Pattern: "DELETE " + Prefix + "/tags/{name}", Summary: "Deletes a tag", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted", 404: "Error"}},
	{Pattern: "POST " + Prefix + "/tags/associate/{name}", Summary: "Tags the components the search parameters select", Tag: nx, Auth: nxAdmin, Query: []string{"repository", "format", "group", "name", "version", "q", "tag"}, Reply: "NexusTagAssociation"},
	{Pattern: "DELETE " + Prefix + "/tags/associate/{name}", Summary: "Removes the tag from the selected components", Tag: nx, Auth: nxAdmin, Query: []string{"repository", "format", "group", "name", "version", "q", "tag"}, Reply: "NexusTagAssociation"},
	{Pattern: "POST " + Prefix + "/staging/move/{destination}", Summary: "Moves the selected components to another hosted repository of the same format", Tag: nx, Auth: nxAdmin, Query: []string{"repository", "format", "group", "name", "version", "q", "tag"}, Reply: "NexusStagingResult", Replies: map[int]string{400: "Error", 403: "Error", 404: "Error", 409: "NexusStagingResult"}},
	{Pattern: "POST " + Prefix + "/staging/delete", Summary: "Deletes the selected components", Tag: nx, Auth: nxAdmin, Query: []string{"repository", "format", "group", "name", "version", "q", "tag"}, Reply: "NexusStagingResult", Replies: map[int]string{400: "Error", 403: "Error", 404: "Error", 409: "NexusStagingResult"}},
	{Pattern: Prefix + "/", Summary: "Anything else under the prefix: 404 {\"id\": \"*\", \"message\"}", Tag: nx, Replies: map[int]string{404: "Error"}},
	{Pattern: "POST " + Prefix + "/blobstores/azure", Summary: "Creates an Azure blob store (name, softQuota, bucketConfiguration: accountName, containerName, authentication)", Tag: nx, Auth: nxAdmin, Request: "NexusAzureBlobStore", Replies: map[int]string{204: "created", 400: "NexusValidation", 409: "Error"}},
	{Pattern: "GET " + Prefix + "/blobstores/azure/{name}", Summary: "One Azure blob store, without the account key", Tag: nx, Auth: nxAdmin, Reply: "NexusAzureBlobStore", Replies: map[int]string{404: "Error"}},
	{Pattern: "PUT " + Prefix + "/blobstores/azure/{name}", Summary: "Replaces an Azure blob store's soft quota and authentication (the account and container cannot change)", Tag: nx, Auth: nxAdmin, Request: "NexusAzureBlobStore", Replies: map[int]string{204: "updated", 400: "NexusValidation", 404: "Error"}},
	{Pattern: "GET " + Prefix + "/blobstores/file/{name}", Summary: "One file blob store: path (its name for the default location) and softQuota", Tag: nx, Auth: nxAdmin, Reply: "NexusFileBlobStore", Replies: map[int]string{400: "Error", 404: "Error"}},
	{Pattern: "DELETE " + Prefix + "/blobstores/{name}", Summary: "Deletes a blob store no repository or group uses (in use: 400)", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted", 400: "Error", 404: "Error"}},
	{Pattern: "POST " + Prefix + "/blobstores/s3", Summary: "Creates an S3 blob store (name, softQuota, bucketConfiguration: bucket, security, encryption, advancedConnection, failoverBuckets)", Tag: nx, Auth: nxAdmin, Request: "NexusS3BlobStore", Replies: map[int]string{201: "created", 400: "NexusValidation", 409: "Error"}},
	{Pattern: "GET " + Prefix + "/blobstores/s3/{name}", Summary: "One S3 blob store; the secret key is the placeholder", Tag: nx, Auth: nxAdmin, Reply: "NexusS3BlobStore", Replies: map[int]string{400: "Error", 404: "Error"}},
	{Pattern: "PUT " + Prefix + "/blobstores/s3/{name}", Summary: "Replaces an S3 blob store's softQuota; the bucket settings must stay as they are (the placeholder keeps the secret)", Tag: nx, Auth: nxAdmin, Request: "NexusS3BlobStore", Replies: map[int]string{204: "updated", 400: "NexusValidation", 404: "Error"}},
	{Pattern: "GET " + Prefix + "/email", Summary: "The e-mail settings in Nexus's shape, without the password", Tag: nx, Auth: nxAdmin, Reply: "NexusEmail"},
	{Pattern: "PUT " + Prefix + "/email", Summary: "Replaces the e-mail settings (STARTTLS is always required; TLS always checks the server identity)", Tag: nx, Auth: nxAdmin, Request: "NexusEmail", Replies: map[int]string{204: "updated", 400: "NexusValidation"}},
	{Pattern: "DELETE " + Prefix + "/email", Summary: "Resets the e-mail settings (disabled, no server, no password)", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "reset"}},
	{Pattern: "POST " + Prefix + "/email/verify", Summary: "Sends a test message to the address in the body: {success, reason}", Tag: nx, Auth: nxAdmin, Reply: "NexusEmailValidation", Replies: map[int]string{400: "NexusEmailValidation"}},
	{Pattern: "GET /service/metrics/", Summary: "Nexus's old metrics paths: 301 to /service/rest/metrics/*, healthcheck to /service/rest/v1/status/check", Tag: nx, Replies: map[int]string{301: "moved"}},
	{Pattern: "GET /service/rest/metrics/ping", Summary: "pong (text), with the rule of /-/metrics/prometheus", Tag: nx, Auth: "admin, or anyone with metricsAnonymous"},
	{Pattern: "GET /service/rest/metrics/prometheus", Summary: "/-/metrics/prometheus; name[] keeps the named metrics", Tag: nx, Auth: "admin, or anyone with metricsAnonymous", Query: []string{"name[]"}},
	{Pattern: "GET /service/rest/metrics/data", Summary: "Runtime metrics in the Dropwizard JSON shape Nexus serves", Tag: nx, Auth: "admin, or anyone with metricsAnonymous", Query: []string{"pretty", "download"}, Reply: "NexusMetricsData"},
	{Pattern: "GET /service/rest/metrics/threads", Summary: "The goroutine dump (text)", Tag: nx, Auth: nxAdmin, Query: []string{"download"}},
	{Pattern: "/service/rest/beta/", Summary: "Nexus's deprecated Beta twins (repositories, security, routing-rules, email, blobstores) served by their v1 routes; GET /beta/repositories is repositorySettings", Tag: nx, Replies: map[int]string{404: "Error"}},
	{Pattern: "GET " + Prefix + "/configuration", Summary: "Exports the configuration (Command Center's document, JSON, without secrets); needs X-Nexus-Migration-Protocol-Version: 1", Tag: nx, Auth: nxAdmin, Replies: map[int]string{200: "the document", 400: "Error"}},
	{Pattern: "PUT " + Prefix + "/configuration", Summary: "Uploads a configuration document: creates what is missing, overwrites nothing", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "applied", 400: "NexusValidation", 409: "ConfigurationResult"}},
}

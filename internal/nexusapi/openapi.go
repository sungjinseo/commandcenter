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
	{Pattern: "GET " + Prefix + "/status/writable", Summary: "Readiness: 200 when the database answers, else 503", Tag: nx},
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
	{Pattern: "GET " + Prefix + "/assets", Summary: "Assets of a repository, paged", Tag: nx, Query: []string{"repository", "continuationToken"}, Reply: "NexusAssetPage"},
	{Pattern: "GET " + Prefix + "/assets/{id}", Summary: "One asset", Tag: nx, Reply: "NexusAsset"},
	{Pattern: "DELETE " + Prefix + "/assets/{id}", Summary: "Deletes a raw file (other formats: use components)", Tag: nx, Auth: "a user with delete", Replies: map[int]string{204: "deleted", 400: "Error"}},
	{Pattern: "GET " + Prefix + "/search", Summary: "Component search", Tag: nx, Query: []string{"q", "repository", "format", "group", "name", "version", "sort", "direction", "continuationToken"}, Reply: "NexusComponentPage"},
	{Pattern: "GET " + Prefix + "/search/assets", Summary: "Asset search", Tag: nx, Query: []string{"q", "repository", "format", "group", "name", "version", "sort", "direction", "continuationToken"}, Reply: "NexusAssetPage"},
	{Pattern: "GET " + Prefix + "/search/assets/download", Summary: "Redirects to the one matching asset", Tag: nx, Query: []string{"q", "repository", "format", "group", "name", "version", "sort", "direction"}, Replies: map[int]string{302: "redirect", 400: "Error", 404: "Error"}},
	{Pattern: "GET " + Prefix + "/security/users", Summary: "Stored users", Tag: nx, Auth: nxAdmin, Query: []string{"userId"}, Reply: "NexusUserList"},
	{Pattern: "GET " + Prefix + "/security/roles", Summary: "Roles", Tag: nx, Auth: nxAdmin, Reply: "NexusRoleList"},
	{Pattern: "GET " + Prefix + "/blobstores", Summary: "Blob stores with softQuota", Tag: nx, Auth: nxAdmin, Reply: "NexusBlobStoreList"},
	{Pattern: "GET " + Prefix + "/blobstores/{name}/quota-status", Summary: "Soft quota state: isViolation, message, blobStoreName", Tag: nx, Auth: nxAdmin, Reply: "NexusQuotaStatus"},
	{Pattern: "POST " + Prefix + "/blobstores/file", Summary: "Creates a file blob store: name, path, softQuota {type, limit (MB)}", Tag: nx, Auth: nxAdmin, Request: "NexusFileBlobStore", Replies: map[int]string{204: "created", 400: "NexusValidation"}},
	{Pattern: "PUT " + Prefix + "/blobstores/file/{name}", Summary: "Updates a file blob store's softQuota (path is immutable)", Tag: nx, Auth: nxAdmin, Request: "NexusFileBlobStore", Replies: map[int]string{204: "updated", 400: "NexusValidation"}},
	{Pattern: "GET " + Prefix + "/tasks", Summary: "Scheduled tasks", Tag: nx, Auth: nxAdmin, Reply: "NexusTaskPage"},
	{Pattern: "GET " + Prefix + "/routing-rules", Summary: "Routing rules", Tag: nx, Auth: nxAdmin, Reply: "NexusRoutingRuleList"},
	{Pattern: "POST " + Prefix + "/routing-rules", Summary: "Creates a routing rule", Tag: nx, Auth: nxAdmin, Request: "NexusRoutingRule", Replies: map[int]string{201: "created"}},
	{Pattern: "GET " + Prefix + "/routing-rules/{name}", Summary: "One routing rule", Tag: nx, Auth: nxAdmin, Reply: "NexusRoutingRule"},
	{Pattern: "PUT " + Prefix + "/routing-rules/{name}", Summary: "Updates a routing rule", Tag: nx, Auth: nxAdmin, Request: "NexusRoutingRule", Replies: map[int]string{204: "updated"}},
	{Pattern: "DELETE " + Prefix + "/routing-rules/{name}", Summary: "Deletes a routing rule", Tag: nx, Auth: nxAdmin, Replies: map[int]string{204: "deleted"}},
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
	{Pattern: Prefix + "/", Summary: "Anything else under the prefix: 404 {\"message\"}", Tag: nx, Replies: map[int]string{404: "Error"}},
}

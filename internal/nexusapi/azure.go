package nexusapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"commandcenter/internal/blob"
	"commandcenter/internal/meta"
)

// Azure blob stores in Nexus's shape: {"name", "softQuota", "bucketConfiguration":
// {"accountName", "containerName", "authentication": {"authenticationMethod",
// "accountKey"}}} (with Command Center's prefix and endpoint beside the
// account), the methods upper-cased (ACCOUNTKEY, MANAGEDIDENTITY and
// Command Center's others: SASTOKEN, CLIENTSECRET, WORKLOADIDENTITY, which
// take their secrets and ids through /-/api/blobstores). The account key is
// never returned. Writes are the UI's /-/api/blobstores calls.

func (h *Handler) azureBlobRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("POST "+p+"/blobstores/azure", h.createAzureBlobStore)
	m.HandleFunc("GET "+p+"/blobstores/azure/{name}", h.getAzureBlobStore)
	m.HandleFunc("PUT "+p+"/blobstores/azure/{name}", h.updateAzureBlobStore)
}

type nexusAzure struct {
	Name      string      `json:"name"`
	SoftQuota *nexusQuota `json:"softQuota"`
	Bucket    struct {
		AccountName    string `json:"accountName"`
		ContainerName  string `json:"containerName"`
		Prefix         string `json:"prefix,omitempty"`   // Command Center's: the blob name prefix
		Endpoint       string `json:"endpoint,omitempty"` // Command Center's: Azurite, sovereign clouds, private endpoints
		Authentication struct {
			Method     string `json:"authenticationMethod"`
			AccountKey string `json:"accountKey,omitempty"`
		} `json:"authentication"`
	} `json:"bucketConfiguration"`
}

// uiAzure is the part of the UI's blob-store view an azure answer needs.
type uiAzure struct {
	Name  string           `json:"name"`
	Type  string           `json:"type"`
	Quota *uiQuota         `json:"quota"`
	Azure blob.AzureConfig `json:"azure"` // without the secrets
}

// azureMethod is the Command Center method of a Nexus one, matched without
// case; an unknown one goes through for the UI to refuse.
func azureMethod(nx string) string {
	for _, m := range blob.AzureMethods {
		if strings.EqualFold(m, nx) {
			return m
		}
	}
	return nx
}

// azureStore fetches the named azure store from the UI's API; it answers 404
// itself for a missing store or one of another type.
func (h *Handler) azureStore(w http.ResponseWriter, r *http.Request, name string) (uiAzure, bool) {
	status, body := h.call(r, http.MethodGet, "/-/api/blobstores/"+url.PathEscape(name), nil)
	var s uiAzure
	json.Unmarshal(body, &s)
	switch {
	case status == http.StatusOK && s.Type == meta.BlobStoreAzure:
		return s, true
	case status == http.StatusOK || status == http.StatusNotFound:
		writeMessage(w, http.StatusNotFound, "azure blob store not found")
	default:
		writeUIError(w, status, body)
	}
	return uiAzure{}, false
}

func (h *Handler) getAzureBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	s, ok := h.azureStore(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	var out nexusAzure
	out.Name, out.SoftQuota = s.Name, softQuota(s.Quota)
	out.Bucket.AccountName, out.Bucket.ContainerName = s.Azure.AccountName, s.Azure.ContainerName
	out.Bucket.Prefix, out.Bucket.Endpoint = s.Azure.Prefix, s.Azure.Endpoint
	out.Bucket.Authentication.Method = strings.ToUpper(s.Azure.Auth)
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) createAzureBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusAzure
	if !decodeSmall(w, r, &in) {
		return
	}
	a := in.Bucket.Authentication
	status, body := h.call(r, http.MethodPost, "/-/api/blobstores", map[string]any{"name": in.Name, "type": meta.BlobStoreAzure,
		"quota": in.SoftQuota.ui(), "azure": blob.AzureConfig{AccountName: in.Bucket.AccountName, ContainerName: in.Bucket.ContainerName,
			Prefix: in.Bucket.Prefix, Endpoint: in.Bucket.Endpoint, Auth: azureMethod(a.Method), AccountKey: a.AccountKey}})
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateAzureBlobStore replaces the soft quota and the authentication; the
// ids and endpoints only the UI's API sets are kept, as is the account key
// when none is sent under the same method.
func (h *Handler) updateAzureBlobStore(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusAzure
	if !decodeSmall(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	cur, ok := h.azureStore(w, r, name)
	if !ok {
		return
	}
	c := cur.Azure
	// The location as sent: the UI keeps an empty field and refuses a change.
	c.AccountName, c.ContainerName, c.Prefix, c.Endpoint = in.Bucket.AccountName, in.Bucket.ContainerName, in.Bucket.Prefix, in.Bucket.Endpoint
	c.Auth, c.AccountKey = azureMethod(in.Bucket.Authentication.Method), in.Bucket.Authentication.AccountKey
	status, body := h.call(r, http.MethodPut, "/-/api/blobstores/"+url.PathEscape(name), map[string]any{"quota": in.SoftQuota.ui(), "azure": c})
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

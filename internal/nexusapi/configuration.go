package nexusapi

import (
	"encoding/json"
	"io"
	"net/http"
)

// Nexus Repository's configuration API (3.87, help.sonatype.com
// "Configuration API"): GET /v1/configuration exports the configuration,
// PUT /v1/configuration uploads such a document and does not overwrite
// what exists. Nexus does not publish the document's shape; this one is
// Command Center's (GET /-/api/configuration, JSON, without secrets), and
// the upload is its apply with createOnly: missing objects are created,
// existing ones and the settings are left as they are.

// migrationHeader is required by Nexus's export (400 without it).
const migrationHeader = "X-Nexus-Migration-Protocol-Version"

func (h *Handler) configRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/configuration", h.exportConfiguration)
	m.HandleFunc("PUT "+p+"/configuration", h.uploadConfiguration)
}

func (h *Handler) exportConfiguration(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(migrationHeader) != "1" {
		writeMessage(w, http.StatusBadRequest, migrationHeader+": 1 is required")
		return
	}
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodGet, "/-/api/configuration?format=json", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (h *Handler) uploadConfiguration(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	doc, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeMessage(w, http.StatusRequestEntityTooLarge, "the configuration is larger than 8 MiB")
		return
	}
	if !json.Valid(doc) {
		writeMessage(w, http.StatusBadRequest, "the configuration must be JSON")
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/configuration?createOnly=true", json.RawMessage(doc))
	switch status {
	case http.StatusOK:
		w.WriteHeader(http.StatusNoContent)
	case http.StatusConflict: // a step failed: the plan says which
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	default:
		writeUIError(w, status, body)
	}
}

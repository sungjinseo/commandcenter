package nexusapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The SSL trust store (Nexus's /security/ssl): the UI's /-/api/ssl/
// endpoints in Nexus's shapes.

// uiCert is the UI's certificate view.
type uiCert struct {
	ID, Subject, Issuer, SubjectCommonName, SubjectOrganization, SubjectOrganizationalUnit string
	IssuerCommonName, IssuerOrganization, IssuerOrganizationalUnit, SerialNumber           string
	NotBefore, NotAfter, PEM                                                               string
}

func nexusCert(c uiCert) map[string]any {
	ms := func(s string) int64 {
		t, _ := time.Parse(time.RFC3339, s)
		return t.UnixMilli()
	}
	return map[string]any{
		"id": c.ID, "fingerprint": c.ID, "pem": c.PEM, "serialNumber": c.SerialNumber,
		"subjectCommonName": c.SubjectCommonName, "subjectOrganization": c.SubjectOrganization, "subjectOrganizationalUnit": c.SubjectOrganizationalUnit,
		"issuerCommonName": c.IssuerCommonName, "issuerOrganization": c.IssuerOrganization, "issuerOrganizationalUnit": c.IssuerOrganizationalUnit,
		"issuedOn": ms(c.NotBefore), "expiresOn": ms(c.NotAfter),
	}
}

// GET /security/ssl: the trust store; with ?host=&port= the certificate
// the host presents (its leaf), as Nexus's "retrieve" does.
func (h *Handler) sslList(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	var status int
	var body []byte
	if host := q.Get("host"); host != "" {
		port, _ := strconv.Atoi(q.Get("port"))
		if q.Get("port") == "" {
			port = 443
		}
		status, body = h.call(r, http.MethodPost, "/-/api/ssl/retrieve", map[string]any{"host": host, "port": port})
		if status != http.StatusOK {
			writeUIError(w, status, body)
			return
		}
		var res struct{ Certificates []uiCert }
		json.Unmarshal(body, &res)
		if len(res.Certificates) == 0 {
			writeMessage(w, http.StatusBadGateway, "no certificate received")
			return
		}
		writeJSON(w, http.StatusOK, nexusCert(res.Certificates[0]))
		return
	}
	status, body = h.call(r, http.MethodGet, "/-/api/ssl/certificates", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	var res struct{ Certificates []uiCert }
	json.Unmarshal(body, &res)
	out := []map[string]any{}
	for _, c := range res.Certificates {
		out = append(out, nexusCert(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /security/ssl/truststore: the body is the PEM certificate; 201 with
// the certificate added, 409 when it is there already.
func (h *Handler) sslAdd(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	pem, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeMessage(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	status, body := h.call(r, http.MethodPost, "/-/api/ssl/certificates", map[string]any{"pem": string(pem)})
	if status != http.StatusCreated {
		writeUIError(w, status, body)
		return
	}
	var res struct{ Certificates []uiCert }
	json.Unmarshal(body, &res)
	if len(res.Certificates) == 0 {
		writeMessage(w, http.StatusInternalServerError, "no certificate added")
		return
	}
	writeJSON(w, http.StatusCreated, nexusCert(res.Certificates[0]))
}

// DELETE /security/ssl/truststore/{id}: 204, or 404.
func (h *Handler) sslRemove(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	status, body := h.call(r, http.MethodDelete, "/-/api/ssl/certificates/"+r.PathValue("id"), nil)
	if status != http.StatusNoContent {
		writeUIError(w, status, body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

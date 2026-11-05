package nexusapi

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"
)

// What clients written for Nexus ask about the server itself before they
// use its repositories: which Nexus it is (Sonatype's client library, in
// Jenkins's Nexus Platform plugin) and the Groovy script API (nexus3-cli,
// Ansible roles), which this server does not have.

// The Nexus this API answers as. Clients gate features on both values:
// Jenkins's Nexus Platform plugin warns about anything but a PRO of 3.13.0
// or newer and hides its tag fields from it. nexusVersion is the release
// whose REST API this package follows; the edition says that the Pro
// endpoints (tags, staging, cleanup policies) are served.
const (
	nexusVersion = "3.91.1-04"
	nexusEdition = "PRO"
)

func (h *Handler) clientRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET /service/rest/wonderland/status", h.wonderlandStatus) // outside the prefix; the server routes it here
	m.HandleFunc("GET "+p+"/script", h.listScripts)
	m.HandleFunc("POST "+p+"/script", h.noScript(http.StatusGone))
	m.HandleFunc(p+"/script/", h.noScript(http.StatusNotFound))
}

// wonderlandStatus is GET /service/rest/wonderland/status (signed in), the
// first request of Sonatype's client library ("Test connection" of a
// Nexus Repository Manager 3.x server in Jenkins): the edition and
// version, as XML, which that client parses and Nexus answers to a request
// without Accept, or as JSON when the request asks for that alone.
func (h *Handler) wonderlandStatus(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	st := struct {
		XMLName xml.Name `xml:"status" json:"-"`
		Edition string   `xml:"edition" json:"edition"`
		Version string   `xml:"version" json:"version"`
	}{Edition: nexusEdition, Version: nexusVersion}
	if a := r.Header.Get("Accept"); strings.Contains(a, "json") && !strings.Contains(a, "xml") {
		writeJSON(w, http.StatusOK, st)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, xml.Header)
	xml.NewEncoder(w).Encode(st)
}

// noScripts is why the script API holds nothing: a Nexus script is Groovy
// code that runs inside the server with all its rights.
const noScripts = "Command Center has no script API (a script would run arbitrary code inside the server): use the REST API under " + Prefix + " instead"

// The script API answers as a Nexus that holds no script and has script
// creation turned off (Nexus's default since 3.21.2): the list is empty,
// creating a script is a 410 and a named one (read, update, delete, run)
// a 404, both with the reason as their message. A client that installs
// its scripts first (nexus3-cli does, for every repository command) stops
// there and shows it.
func (h *Handler) listScripts(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) {
		writeJSON(w, http.StatusOK, []any{})
	}
}

func (h *Handler) noScript(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.signedIn(w, r) {
			writeMessage(w, status, noScripts)
		}
	}
}

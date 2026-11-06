package nexusapi

import "net/http"

// The parts of Nexus's API that are left out on purpose. A client that
// asks for one (the Terraform provider's nexus_capability, nexus_iqserver
// and nexus_security_oidc) reads why and what to use, where the catch-all
// would only say "not found".

const (
	noCapabilities = "Command Center has no capabilities. What Nexus sets with them (the base URL, webhooks, remote user authentication) is in the settings, the webhooks and the realms here: /service/rest/swagger.json lists the routes."
	noIQ           = "Command Center has no IQ Server integration. Proxies are screened with OSV vulnerability data instead: a proxy repository's firewall setting."
	noOAuth2       = "Command Center signs in with OpenID Connect by discovery from the issuer (issuer and client id), which this body of endpoint URLs and keys cannot express. Use PUT /-/api/realms/oidc or the Realms screen."
)

func (h *Handler) leftOutRoutes(m *http.ServeMux, p string) {
	say := func(status int, msg string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { writeMessage(w, status, msg) }
	}
	m.HandleFunc(p+"/capabilities", say(http.StatusNotFound, noCapabilities))
	m.HandleFunc(p+"/capabilities/", say(http.StatusNotFound, noCapabilities))
	m.HandleFunc(p+"/iq", say(http.StatusNotFound, noIQ))
	m.HandleFunc(p+"/iq/", say(http.StatusNotFound, noIQ))
	// Settings of another shape: reading is 404 (to the provider, "not
	// configured"), writing a refused request.
	m.HandleFunc(p+"/security/oauth2", func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusNotFound
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			status = http.StatusBadRequest
		}
		writeMessage(w, status, noOAuth2)
	})
}

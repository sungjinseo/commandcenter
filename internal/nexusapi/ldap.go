package nexusapi

import (
	"cmp"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"commandcenter/internal/meta"
)

// Nexus's LDAP servers (/security/ldap) over Command Center's one directory
// (/-/api/realms/ldap): the list holds it, or nothing. A body maps onto the
// directory's settings as import-nexus maps a Nexus server: the user base DN
// is userBaseDn and searchBase joined, the filter is made of userObjectClass,
// userIdAttribute and userLdapFilter (a userLdapFilter with {user} is the whole
// filter, which is how a directory set up in the web UI reads). What a body
// has no field for (group mappings, default roles, StartTLS, skipping
// certificate verification) stays. What cannot be honoured is refused.
//
// The rest of a body changes nothing here and reads back as written
// (nexusLDAPKey): the name, useTrustStore (the SSL trust store is always
// used), the retry settings and the settings of static groups while
// ldapGroupsAsRoles is off, when Nexus does not use them either.

// nexusLDAPKey keeps the body last written through this API, without its
// password: it is read back while it still maps onto what the directory holds
// (the web UI can change that), and else derived from the directory.
const nexusLDAPKey = "nexusapi.ldap"

func (h *Handler) ldapRoutes(m *http.ServeMux, p string) {
	m.HandleFunc("GET "+p+"/security/ldap", h.listLDAP)
	m.HandleFunc("POST "+p+"/security/ldap", h.createLDAP)
	m.HandleFunc("POST "+p+"/security/ldap/change-order", h.orderLDAP)
	m.HandleFunc("GET "+p+"/security/ldap/{name}", h.getLDAP)
	m.HandleFunc("PUT "+p+"/security/ldap/{name}", h.updateLDAP)
	m.HandleFunc("DELETE "+p+"/security/ldap/{name}", h.deleteLDAP)
}

// nexusLDAP is Nexus's body, request and answer. The answer has no password.
type nexusLDAP struct {
	ID                          string `json:"id"`
	Order                       int    `json:"order"`
	Name                        string `json:"name"`
	Protocol                    string `json:"protocol"`
	UseTrustStore               bool   `json:"useTrustStore"`
	Host                        string `json:"host"`
	Port                        int    `json:"port"`
	SearchBase                  string `json:"searchBase"`
	AuthScheme                  string `json:"authScheme"`
	AuthRealm                   string `json:"authRealm"`
	AuthUsername                string `json:"authUsername"`
	AuthPassword                string `json:"authPassword,omitempty"`
	ConnectionTimeoutSeconds    int    `json:"connectionTimeoutSeconds"`
	ConnectionRetryDelaySeconds int    `json:"connectionRetryDelaySeconds"`
	MaxIncidentsCount           int    `json:"maxIncidentsCount"`
	UserBaseDN                  string `json:"userBaseDn"`
	UserSubtree                 bool   `json:"userSubtree"`
	UserObjectClass             string `json:"userObjectClass"`
	UserLDAPFilter              string `json:"userLdapFilter"`
	UserIDAttribute             string `json:"userIdAttribute"`
	UserRealNameAttribute       string `json:"userRealNameAttribute"`
	UserEmailAddressAttribute   string `json:"userEmailAddressAttribute"`
	UserPasswordAttribute       string `json:"userPasswordAttribute"`
	LDAPGroupsAsRoles           bool   `json:"ldapGroupsAsRoles"`
	GroupType                   string `json:"groupType"`
	GroupBaseDN                 string `json:"groupBaseDn"`
	GroupSubtree                bool   `json:"groupSubtree"`
	GroupObjectClass            string `json:"groupObjectClass"`
	GroupIDAttribute            string `json:"groupIdAttribute"`
	GroupMemberAttribute        string `json:"groupMemberAttribute"`
	GroupMemberFormat           string `json:"groupMemberFormat"`
	UserMemberOfAttribute       string `json:"userMemberOfAttribute"`
}

// uiLDAP is the UI's view of the directory, and its request with the two
// password fields.
type uiLDAP struct {
	URL                string              `json:"url"`
	StartTLS           bool                `json:"startTLS"`
	InsecureSkipVerify bool                `json:"insecureSkipVerify"`
	BindDN             string              `json:"bindDN"`
	BindPassword       string              `json:"bindPassword,omitempty"`
	ClearBindPassword  bool                `json:"clearBindPassword,omitempty"`
	TimeoutSeconds     int                 `json:"timeoutSeconds"`
	UserBaseDN         string              `json:"userBaseDN"`
	UserFilter         string              `json:"userFilter"`
	UserIDAttr         string              `json:"userIdAttr"`
	NameAttr           string              `json:"nameAttr"`
	EmailAttr          string              `json:"emailAttr"`
	GroupAttr          string              `json:"groupAttr"`
	GroupMappings      []meta.GroupMapping `json:"groupMappings"`
	DefaultRoles       []string            `json:"defaultRoles"`
	UserOneLevel       bool                `json:"userOneLevel"`
	GroupsAsRoles      bool                `json:"groupsAsRoles"`
}

// directory is what body n makes of the directory's settings cur, or the
// values Command Center cannot honour, as "field: why".
func (n nexusLDAP) directory(cur uiLDAP) (uiLDAP, []string) {
	var errs []string
	refuse := func(bad bool, msg string) {
		if bad {
			errs = append(errs, msg)
		}
	}
	scheme, auth := strings.ToLower(n.Protocol), strings.ToUpper(n.AuthScheme)
	refuse(n.Name == "", "name: a name is required")
	refuse(scheme != "ldap" && scheme != "ldaps", "protocol: ldap or ldaps")
	refuse(n.Host == "", "host: a host is required")
	refuse(n.Port < 1 || n.Port > 65535, "port: must be between 1 and 65535")
	refuse(auth != "SIMPLE" && auth != "NONE", "authScheme: the service bind is SIMPLE or NONE here; CRAM_MD5 and DIGEST_MD5 are not spoken")
	refuse(n.UserPasswordAttribute != "", "userPasswordAttribute: a password is checked by binding as the user, never by reading an attribute")
	refuse(n.LDAPGroupsAsRoles && strings.EqualFold(n.GroupType, "static"),
		"groupType: groups are read from an attribute of the user (dynamic, userMemberOfAttribute), not from group entries: use dynamic with a directory that fills that attribute")
	def := meta.DefaultLDAP()
	c := cur
	c.URL = scheme + "://" + net.JoinHostPort(n.Host, strconv.Itoa(n.Port))
	c.StartTLS = cur.StartTLS && scheme == "ldap"
	c.BindDN = ""
	if auth == "SIMPLE" {
		c.BindDN = n.AuthUsername
	}
	c.TimeoutSeconds = def.TimeoutSeconds
	if t := n.ConnectionTimeoutSeconds; t > 0 { // as import-nexus: a longer wait is the longest here
		c.TimeoutSeconds = min(t, meta.MaxLDAPTimeoutSeconds)
	}
	c.UserBaseDN = strings.Trim(n.UserBaseDN+","+n.SearchBase, ",")
	c.UserIDAttr = cmp.Or(n.UserIDAttribute, def.UserIDAttr)
	c.UserFilter = n.filter(c.UserIDAttr)
	c.NameAttr = cmp.Or(n.UserRealNameAttribute, def.NameAttr)
	c.EmailAttr = cmp.Or(n.UserEmailAddressAttribute, def.EmailAttr)
	c.GroupAttr = cmp.Or(n.UserMemberOfAttribute, def.GroupAttr)
	c.UserOneLevel, c.GroupsAsRoles = !n.UserSubtree, n.LDAPGroupsAsRoles
	return c, errs
}

// filter is the directory's user filter of body n.
func (n nexusLDAP) filter(idAttr string) string {
	f := strings.TrimSpace(n.UserLDAPFilter)
	if strings.Contains(f, "{user}") {
		return f
	}
	parts := []string{"(" + idAttr + "={user})"}
	if n.UserObjectClass != "" {
		parts = append([]string{"(objectClass=" + n.UserObjectClass + ")"}, parts...)
	}
	if f != "" {
		if !strings.HasPrefix(f, "(") {
			f = "(" + f + ")"
		}
		parts = append(parts, f)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(&" + strings.Join(parts, "") + ")"
}

// derivedLDAP is the directory cur in Nexus's shape.
func derivedLDAP(cur uiLDAP) nexusLDAP {
	n := nexusLDAP{Name: "ldap", ID: "ldap", Order: 1, UseTrustStore: true, AuthScheme: "NONE", AuthUsername: cur.BindDN,
		ConnectionTimeoutSeconds: cur.TimeoutSeconds, ConnectionRetryDelaySeconds: 300, MaxIncidentsCount: 3,
		SearchBase: cur.UserBaseDN, UserSubtree: !cur.UserOneLevel, UserLDAPFilter: cur.UserFilter, UserIDAttribute: cur.UserIDAttr,
		UserRealNameAttribute: cur.NameAttr, UserEmailAddressAttribute: cur.EmailAttr, LDAPGroupsAsRoles: cur.GroupsAsRoles,
		GroupType: "dynamic", UserMemberOfAttribute: cur.GroupAttr}
	if cur.BindDN != "" {
		n.AuthScheme = "SIMPLE"
	}
	if cur.UserFilter == "("+cur.UserIDAttr+"={user})" { // only the id attribute: that field says it
		n.UserLDAPFilter = ""
	}
	if u, err := url.Parse(cur.URL); err == nil {
		n.Protocol, n.Host = u.Scheme, u.Hostname()
		n.Port, _ = strconv.Atoi(u.Port())
		if n.Port == 0 {
			n.Port = map[string]int{"ldap": 389, "ldaps": 636}[u.Scheme]
		}
	}
	return n
}

// storedLDAP reads the directory's settings and its answer in Nexus's shape
// (server is nil without a directory); it answers a failure itself.
func (h *Handler) storedLDAP(w http.ResponseWriter, r *http.Request) (cur uiLDAP, server *nexusLDAP, ok bool) {
	status, body := h.call(r, http.MethodGet, "/-/api/realms/ldap", nil)
	if status != http.StatusOK {
		writeUIError(w, status, body)
		return cur, nil, false
	}
	json.Unmarshal(body, &cur)
	if strings.TrimSpace(cur.URL) == "" {
		return cur, nil, true
	}
	n := derivedLDAP(cur)
	var kept nexusLDAP
	if v, err := h.DB.GetSetting(r.Context(), nexusLDAPKey); err == nil && json.Unmarshal([]byte(v), &kept) == nil && kept.Name != "" {
		n.Name, n.ID = kept.Name, kept.Name
		if c, errs := kept.directory(cur); len(errs) == 0 && reflect.DeepEqual(c, cur) {
			n = kept
		}
	}
	return cur, &n, true
}

// putLDAP stores body n as the directory; it answers a refusal itself.
func (h *Handler) putLDAP(w http.ResponseWriter, r *http.Request, n nexusLDAP, cur uiLDAP) bool {
	c, errs := n.directory(cur)
	if len(errs) > 0 {
		writeValidation(w, errs)
		return false
	}
	if c.BindDN == "" { // an anonymous search
		c.ClearBindPassword = true
	} else {
		c.BindPassword = n.AuthPassword // empty keeps the stored one, unless the server changed
	}
	if status, body := h.call(r, http.MethodPut, "/-/api/realms/ldap", c); status != http.StatusOK {
		writeUIError(w, status, body)
		return false
	}
	n.ID, n.Order, n.AuthPassword = n.Name, 1, ""
	b, _ := json.Marshal(n)
	if err := h.DB.SetSetting(r.Context(), nexusLDAPKey, string(b)); err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}

func (h *Handler) listLDAP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "settings", meta.ActionRead); !ok {
		return
	}
	if _, server, ok := h.storedLDAP(w, r); ok {
		out := []nexusLDAP{}
		if server != nil {
			out = append(out, *server)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// namedLDAP is storedLDAP for the server the path names: 404 for any other.
func (h *Handler) namedLDAP(w http.ResponseWriter, r *http.Request) (uiLDAP, *nexusLDAP, bool) {
	cur, server, ok := h.storedLDAP(w, r)
	if !ok {
		return cur, nil, false
	}
	if name := r.PathValue("name"); server == nil || server.Name != name {
		writeMessage(w, http.StatusNotFound, "LDAP server "+strconv.Quote(name)+" not found")
		return cur, nil, false
	}
	return cur, server, true
}

func (h *Handler) getLDAP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.app(w, r, "settings", meta.ActionRead); !ok {
		return
	}
	if _, server, ok := h.namedLDAP(w, r); ok {
		writeJSON(w, http.StatusOK, *server)
	}
}

func (h *Handler) createLDAP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusLDAP
	if !decodeSmall(w, r, &in) {
		return
	}
	cur, server, ok := h.storedLDAP(w, r)
	if !ok {
		return
	}
	if server != nil {
		writeMessage(w, http.StatusBadRequest, "Command Center has one LDAP directory, and it is configured: update "+strconv.Quote(server.Name)+", or delete it first")
		return
	}
	if h.putLDAP(w, r, in, cur) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (h *Handler) updateLDAP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var in nexusLDAP
	if !decodeSmall(w, r, &in) {
		return
	}
	cur, _, ok := h.namedLDAP(w, r)
	if !ok {
		return
	}
	in.Name = cmp.Or(in.Name, r.PathValue("name"))
	if h.putLDAP(w, r, in, cur) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteLDAP resets the directory, the mappings with it, and turns the ldap
// realm off.
func (h *Handler) deleteLDAP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	if _, _, ok := h.namedLDAP(w, r); !ok || !h.realmOff(w, r, meta.RealmLDAP) {
		return
	}
	if status, body := h.call(r, http.MethodPut, "/-/api/realms/ldap", uiLDAP{ClearBindPassword: true}); status != http.StatusOK {
		writeUIError(w, status, body)
		return
	}
	if err := h.DB.SetSetting(r.Context(), nexusLDAPKey, ""); err != nil {
		writeMessage(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// orderLDAP accepts the order of the one server there is.
func (h *Handler) orderLDAP(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r); !ok {
		return
	}
	var names []string
	if !decodeSmall(w, r, &names) {
		return
	}
	_, server, ok := h.storedLDAP(w, r)
	if !ok {
		return
	}
	for _, name := range names {
		if server == nil || name != server.Name {
			writeMessage(w, http.StatusBadRequest, "Command Center has one LDAP directory: there is no server "+strconv.Quote(name)+" to order")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

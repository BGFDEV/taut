package fakegraph

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

const assertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// entraError writes the token endpoint's error shape. Like the real thing,
// the description carries the AADSTS code, trace IDs and a timestamp on
// separate lines.
func (s *Server) entraError(w http.ResponseWriter, status int, oauthErr string, aadsts int, message string) {
	now := s.cfg.now().UTC()
	trace, corr := newGUID(), newGUID()
	ts := now.Format("2006-01-02 15:04:05Z")
	writeJSON(w, status, map[string]any{
		"error": oauthErr,
		"error_description": fmt.Sprintf("AADSTS%d: %s\r\nTrace ID: %s\r\nCorrelation ID: %s\r\nTimestamp: %s",
			aadsts, message, trace, corr, ts),
		"error_codes":    []int{aadsts},
		"timestamp":      ts,
		"trace_id":       trace,
		"correlation_id": corr,
		"error_uri":      fmt.Sprintf("https://login.microsoftonline.com/error?code=%d", aadsts),
	})
}

// handleToken implements POST /{tenant}/oauth2/v2.0/token for the
// client-credentials grant.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	tenant, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/oauth2/v2.0/token")
	if !ok || tenant == "" || strings.Contains(tenant, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		s.entraError(w, http.StatusBadRequest, "invalid_request", 900144, "The request body could not be parsed.")
		return
	}
	form := r.PostForm
	clientID := form.Get("client_id")
	record(r).AppID = clientID

	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.isTenant(tenant) {
		s.entraError(w, http.StatusBadRequest, "invalid_request", 90002,
			fmt.Sprintf("Tenant '%s' not found. Check to make sure you have the correct tenant ID and are signing into the correct cloud.", tenant))
		return
	}
	if gt := form.Get("grant_type"); gt != "client_credentials" {
		s.entraError(w, http.StatusBadRequest, "unsupported_grant_type", 70003,
			fmt.Sprintf("The app requested an unsupported grant type '%s'.", gt))
		return
	}
	if clientID == "" {
		s.entraError(w, http.StatusBadRequest, "invalid_request", 900144,
			"The request body must contain the following parameter: 'client_id'.")
		return
	}
	app := s.st.apps[strings.ToLower(clientID)]
	if app == nil {
		s.entraError(w, http.StatusBadRequest, "unauthorized_client", 700016,
			fmt.Sprintf("Application with identifier '%s' was not found in the directory '%s'. "+
				"This can happen if the application has not been installed by the administrator of the tenant "+
				"or consented to by any user in the tenant. You may have sent your authentication request to the wrong tenant.",
				clientID, s.st.org.DisplayName))
		return
	}
	resource, ok := strings.CutSuffix(form.Get("scope"), "/.default")
	if ru, err := url.Parse(resource); !ok || err != nil || ru.Host == "" || strings.ContainsAny(resource, " ") {
		s.entraError(w, http.StatusBadRequest, "invalid_scope", 1002012,
			fmt.Sprintf("The provided value for scope %s is not valid. Client credential flows must have a scope value "+
				"with /.default suffixed to the resource identifier (application ID URI).", form.Get("scope")))
		return
	}

	switch {
	case form.Has("client_secret"):
		secret := form.Get("client_secret")
		match := func(candidates []string) bool {
			return slices.ContainsFunc(candidates, func(c string) bool {
				return subtle.ConstantTimeCompare([]byte(c), []byte(secret)) == 1
			})
		}
		switch {
		case match(app.Secrets):
		case match(app.ExpiredSecrets):
			s.entraError(w, http.StatusUnauthorized, "invalid_client", 7000222,
				fmt.Sprintf("The provided client secret keys for app '%s' are expired. "+
					"Visit the Azure portal to create new keys for your app, or consider using certificate credentials for added security.", clientID))
			return
		default:
			s.entraError(w, http.StatusUnauthorized, "invalid_client", 7000215,
				fmt.Sprintf("Invalid client secret provided. Ensure the secret being sent in the request is the client secret value, "+
					"not the client secret ID, for a secret added to app '%s'.", clientID))
			return
		}
	case form.Has("client_assertion"):
		if code, msg := s.checkAssertion(r, app, form); code != 0 {
			s.entraError(w, http.StatusUnauthorized, "invalid_client", code, msg)
			return
		}
	default:
		s.entraError(w, http.StatusUnauthorized, "invalid_client", 7000216,
			"'client_assertion', 'client_secret' or 'request' is required for the 'client_credentials' grant type.")
		return
	}

	now := s.cfg.now()
	exp := now.Add(s.cfg.tokenLifetime)
	token := signAccessToken(s.signingKey, accessClaims{
		Aud:   resource,
		Iss:   "https://sts.windows.net/" + s.st.tenantID + "/",
		Iat:   now.Unix(),
		Nbf:   now.Unix(),
		Exp:   exp.Unix(),
		AppID: app.ClientID,
		IDTyp: "app",
		Roles: append([]string{}, app.Roles...),
		Sub:   app.ClientID,
		Tid:   s.st.tenantID,
		Uti:   newGUID(),
		Ver:   "1.0",
	})
	secs := int(s.cfg.tokenLifetime.Seconds())
	w.Header().Set("Cache-Control", "no-store, no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"token_type":     "Bearer",
		"expires_in":     secs,
		"ext_expires_in": secs,
		"access_token":   token,
	})
}

// isTenant accepts the tenant ID or any verified domain name.
func (s *Server) isTenant(name string) bool {
	if strings.EqualFold(name, s.st.tenantID) {
		return true
	}
	return slices.ContainsFunc(s.st.org.Domains, func(d Domain) bool { return strings.EqualFold(d.Name, name) })
}

// checkAssertion validates a certificate client assertion. It returns a
// zero code on success, otherwise the AADSTS code and message.
func (s *Server) checkAssertion(r *http.Request, app *appState, form url.Values) (int, string) {
	if t := form.Get("client_assertion_type"); t != assertionType {
		return 7000216, fmt.Sprintf("The client_assertion_type '%s' is not supported; expected '%s'.", t, assertionType)
	}
	a, err := parseAssertion(form.Get("client_assertion"))
	if err != nil {
		return 50027, "JWT token is invalid or malformed."
	}
	notRegistered := fmt.Sprintf("The certificate with identifier used to sign the client assertion is not registered on application. "+
		"[Reason - The key was not found., Thumbprint of key used by client: '%s', "+
		"Please visit the Azure Portal, Graph Explorer or directly use MS Graph to see configured keys for app Id '%s'.]",
		thumbprintHex(a.X5t), app.ClientID)
	if a.Alg != "RS256" {
		return 700027, "Client assertion failed signature validation. [Reason - Unsupported algorithm '" + a.Alg + "'.]"
	}
	pub, registered := app.certs[strings.TrimRight(a.X5t, "=")]
	if !registered {
		return 700027, notRegistered
	}
	if pub != nil {
		if err := a.verify(pub); err != nil {
			return 700027, "Client assertion failed signature validation. [Reason - The key used is not the registered key.]"
		}
	}
	if !strings.EqualFold(a.Iss, app.ClientID) || !strings.EqualFold(a.Sub, app.ClientID) {
		return 700021, "Client assertion application identifier doesn't match 'client_id' parameter."
	}
	if !audienceMatches(a.Aud, r) {
		return 700023, fmt.Sprintf("Client assertion audience claim does not match Realm issuer. "+
			"The audience '%s' must be this token endpoint.", a.Aud)
	}
	now := s.cfg.now().Unix()
	const skew = 300
	if a.Exp == 0 || now > a.Exp+skew || (a.Nbf != 0 && now < a.Nbf-skew) {
		return 700024, "Client assertion is not within its valid time range. " +
			"Ensure that the access token is not expired before using it for user assertion, or request a new token."
	}
	return 0, ""
}

// audienceMatches accepts the token endpoint URL (or the tenant's v2.0
// issuer path) on this host, under either scheme.
func audienceMatches(aud string, r *http.Request) bool {
	u, err := url.Parse(aud)
	if err != nil || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	got := strings.TrimRight(u.Path, "/")
	base := strings.TrimSuffix(r.URL.Path, "/oauth2/v2.0/token")
	return strings.EqualFold(got, r.URL.Path) || strings.EqualFold(got, base+"/v2.0")
}

// thumbprintHex renders an x5t the way Entra's error messages do.
func thumbprintHex(x5t string) string {
	raw, err := unb64(x5t)
	if err != nil {
		return x5t
	}
	return strings.ToUpper(fmt.Sprintf("%x", raw))
}

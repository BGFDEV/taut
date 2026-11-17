// Package wire holds the small protocol shared by the sidecars and the
// workers that talk to them: the routing headers a worker puts on a
// request to the egress proxy, the authentication description both the
// egress proxy and the token broker understand, and the JSON error envelope.
//
// Nothing here ever carries a secret. An Auth names where a credential lives
// (a secret.Ref) and is resolved only inside the token broker.
package wire

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

// Request headers a worker sets on every proxied request.
const (
	// HeaderUpstream carries the real destination's origin,
	// scheme://host[:port], with no path.
	HeaderUpstream = "X-Taut-Upstream"
	// HeaderAuth carries EncodeAuth of the Auth to apply.
	HeaderAuth = "X-Taut-Auth"
	// HeaderError is set, to the error's code, on every response the
	// egress proxy itself produced (as opposed to one relayed from upstream).
	HeaderError = "X-Taut-Error"
	// HeaderPrefix is the prefix of every header that is addressed to the
	// egress proxy and never forwarded upstream.
	HeaderPrefix = "X-Taut-"
)

// Auth kinds.
const (
	KindEntra = "entra"
	KindSlack = "slack"
	KindNone  = "none"
)

// Bus subjects.
const (
	// SubjectThrottle is where egress proxies publish Throttle events.
	SubjectThrottle = "taut.throttle"
	// SubjectBudgetLease is where the control plane answers lease requests.
	SubjectBudgetLease = "taut.budget.lease"
)

// maxAuthHeader bounds the encoded X-Taut-Auth value.
const maxAuthHeader = 8 << 10

// Credential says where the Entra application's credential lives. Exactly
// one of the two references must be set.
type Credential struct {
	// Certificate references a PEM file (or variable) holding the
	// certificate and its RSA private key.
	Certificate secret.Ref `json:"certificate,omitzero"`
	// ClientSecret references the application's client secret.
	ClientSecret secret.Ref `json:"clientSecret,omitzero"`
}

// Entra identifies an Entra ID application and where its credential lives.
type Entra struct {
	TenantID string `json:"tenantId,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	// Cloud is a name accepted by graph.LookupCloud; empty means global.
	Cloud string `json:"cloud,omitempty"`
	// Authority and GraphBase override the cloud's endpoints. The token
	// broker accepts only overrides its operator has allowed.
	Authority  string     `json:"authority,omitempty"`
	GraphBase  string     `json:"graphBase,omitempty"`
	Credential Credential `json:"credential,omitzero"`
}

// Auth says how a request is to be authenticated. It is the payload of the
// X-Taut-Auth header and the body of the token broker's POST /v1/token.
type Auth struct {
	// Kind is KindEntra, KindSlack or KindNone.
	Kind string `json:"kind"`
	// Entra fields, for KindEntra.
	Entra
	// Token references the Slack token, for KindSlack.
	Token secret.Ref `json:"token,omitzero"`
}

// EntraAuth returns the Auth for an Entra application.
func EntraAuth(e Entra) Auth { return Auth{Kind: KindEntra, Entra: e} }

// SlackAuth returns the Auth for a Slack token reference.
func SlackAuth(ref secret.Ref) Auth { return Auth{Kind: KindSlack, Token: ref} }

// NoAuth returns the Auth for pre-authorized URLs.
func NoAuth() Auth { return Auth{Kind: KindNone} }

// Validate checks the shape of a without resolving anything. A non-nil
// result is a *fail.Error with the given code.
func (a Auth) Validate(code string) error {
	bad := func(why string) error {
		return fail.New(code, "The authentication description is not valid", why,
			"Send kind \"entra\" with tenantId, clientId and exactly one credential, kind \"slack\" with a token reference, or kind \"none\".").
			WithExit(fail.ExitUsage)
	}
	switch a.Kind {
	case KindEntra:
		if !a.Token.IsZero() {
			return bad("kind \"entra\" does not take a token reference")
		}
		if strings.TrimSpace(a.TenantID) == "" || strings.TrimSpace(a.ClientID) == "" {
			return bad("kind \"entra\" needs tenantId and clientId")
		}
		cert, sec := !a.Credential.Certificate.IsZero(), !a.Credential.ClientSecret.IsZero()
		switch {
		case cert && sec:
			return bad("both a certificate and a client secret are referenced; exactly one is allowed")
		case !cert && !sec:
			return bad("neither a certificate nor a client secret is referenced; exactly one is required")
		}
	case KindSlack:
		if a.Entra != (Entra{}) {
			return bad("kind \"slack\" takes only a token reference")
		}
		if a.Token.IsZero() {
			return bad("kind \"slack\" needs a token reference")
		}
	case KindNone:
		if a.Entra != (Entra{}) || !a.Token.IsZero() {
			return bad("kind \"none\" takes no other fields")
		}
	default:
		return bad("kind must be \"entra\", \"slack\" or \"none\"")
	}
	return nil
}

// EncodeAuth renders a as the value of the X-Taut-Auth header: unpadded
// base64url of its JSON.
func EncodeAuth(a Auth) string {
	b, err := json.Marshal(a)
	if err != nil { // unreachable: Auth holds only strings
		panic(fmt.Sprintf("wire: encoding Auth: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeAuth parses an X-Taut-Auth header value and validates its shape.
func DecodeAuth(s string) (Auth, error) {
	const code = "proxy.bad_request"
	if s == "" {
		return Auth{}, errors.New("the " + HeaderAuth + " header is missing")
	}
	if len(s) > maxAuthHeader {
		return Auth{}, errors.New("the " + HeaderAuth + " header is too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Auth{}, errors.New("the " + HeaderAuth + " header is not unpadded base64url")
	}
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return Auth{}, errors.New("the " + HeaderAuth + " header does not hold a JSON object of the expected shape")
	}
	if err := a.Validate(code); err != nil {
		return Auth{}, err
	}
	return a, nil
}

// Headers returns the routing headers for a request to upstream (an origin
// such as https://graph.microsoft.com) authenticated as a.
func Headers(upstream string, a Auth) http.Header {
	h := http.Header{}
	h.Set(HeaderUpstream, upstream)
	h.Set(HeaderAuth, EncodeAuth(a))
	return h
}

// ErrorBody is the JSON envelope both sidecars use for errors.
type ErrorBody struct {
	Error fail.Error `json:"error"`
}

// WriteError writes err as an ErrorBody with the given status.
func WriteError(w http.ResponseWriter, status int, err error) {
	fe := fail.From(err)
	body := ErrorBody{Error: fail.Error{
		Code: fe.Code,
		What: secret.Redact(fe.What),
		Why:  secret.Redact(fe.Why),
		Next: secret.Redact(fe.Next),
	}}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Throttle is the event an egress proxy publishes on SubjectThrottle when an
// upstream tells it to slow down.
type Throttle struct {
	Tenant       string `json:"tenant"`
	Scope        string `json:"scope"`
	RetryAfterMs int64  `json:"retryAfterMs"`
	// At is when the sender saw the response, RFC 3339.
	At     string `json:"at"`
	Holder string `json:"holder"`
}

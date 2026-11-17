package graph

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// newGUID returns a random RFC 4122 version 4 UUID.
func newGUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails; see crypto/rand
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Claims are the access-token claims Taut reads. They are decoded without
// verifying the signature: the token is addressed to Microsoft Graph, which
// is the party that validates it. Use them for reporting, never for
// authorization decisions.
type Claims struct {
	// TenantID is the `tid` claim.
	TenantID string
	// AppID is the application (client) ID: `appid` in v1 tokens, `azp` in v2.
	AppID string
	// Audience is the `aud` claim (the first entry if it is a list).
	Audience string
	// Roles are the application permissions granted with admin consent.
	Roles []string
	// ExpiresAt is the `exp` claim; zero if absent.
	ExpiresAt time.Time
}

// ParseClaims decodes the payload of a JWT access token without verifying
// it. The token never appears in a returned error.
func ParseClaims(accessToken string) (Claims, error) {
	bad := func(why string) (Claims, error) {
		return Claims{}, fail.New("graph.token_unreadable",
			"The access token could not be inspected",
			why,
			"Run `taut auth` again to request a fresh token. If this persists, check that the configured cloud matches the tenant.")
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return bad("It is not a JWT (expected three dot-separated parts).")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return bad("Its payload is not valid base64url.")
	}
	var raw struct {
		Tid   string          `json:"tid"`
		AppID string          `json:"appid"`
		Azp   string          `json:"azp"`
		Aud   json.RawMessage `json:"aud"`
		Roles []string        `json:"roles"`
		Exp   json.Number     `json:"exp"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return bad("Its payload is not a JSON object of the expected shape.")
	}
	c := Claims{TenantID: raw.Tid, AppID: raw.AppID, Roles: raw.Roles}
	if c.AppID == "" {
		c.AppID = raw.Azp
	}
	if len(raw.Aud) > 0 {
		var one string
		var many []string
		if json.Unmarshal(raw.Aud, &one) == nil {
			c.Audience = one
		} else if json.Unmarshal(raw.Aud, &many) == nil && len(many) > 0 {
			c.Audience = many[0]
		}
	}
	if raw.Exp != "" {
		if f, err := raw.Exp.Float64(); err == nil {
			c.ExpiresAt = time.Unix(int64(f), 0).UTC()
		}
	}
	return c, nil
}

// Roles returns the `roles` claim of an access token: the application
// permissions that have been granted. See ParseClaims for caveats.
func Roles(accessToken string) ([]string, error) {
	c, err := ParseClaims(accessToken)
	if err != nil {
		return nil, err
	}
	return c.Roles, nil
}

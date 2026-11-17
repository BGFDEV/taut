package fakegraph

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func newGUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// accessClaims are the claims of tokens this server issues, shaped like an
// Entra v1 app-only access token for Microsoft Graph.
type accessClaims struct {
	Aud   string   `json:"aud"`
	Iss   string   `json:"iss"`
	Iat   int64    `json:"iat"`
	Nbf   int64    `json:"nbf"`
	Exp   int64    `json:"exp"`
	AppID string   `json:"appid"`
	IDTyp string   `json:"idtyp"`
	Roles []string `json:"roles"`
	Sub   string   `json:"sub"`
	Tid   string   `json:"tid"`
	Uti   string   `json:"uti"`
	Ver   string   `json:"ver"`
}

// signAccessToken issues an HS256 JWT signed with the server's key.
func signAccessToken(key []byte, c accessClaims) string {
	h := b64([]byte(`{"typ":"JWT","alg":"HS256"}`))
	p, _ := json.Marshal(c)
	input := h + "." + b64(p)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(input))
	return input + "." + b64(mac.Sum(nil))
}

var errBadToken = errors.New("malformed or forged token")

// verifyAccessToken checks the signature and returns the claims. Expiry is
// the caller's concern.
func verifyAccessToken(key []byte, token string) (accessClaims, error) {
	var c accessClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, errBadToken
	}
	sig, err := unb64(parts[2])
	if err != nil {
		return c, errBadToken
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return c, errBadToken
	}
	payload, err := unb64(parts[1])
	if err != nil || json.Unmarshal(payload, &c) != nil {
		return c, errBadToken
	}
	return c, nil
}

// assertion is a parsed (not yet verified) client assertion.
type assertion struct {
	Alg, X5t      string
	Aud, Iss, Sub string
	Nbf, Exp      int64
	signingInput  string
	signature     []byte
}

func parseAssertion(raw string) (assertion, error) {
	var a assertion
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return a, errors.New("not a three-part JWT")
	}
	hb, err1 := unb64(parts[0])
	pb, err2 := unb64(parts[1])
	sig, err3 := unb64(parts[2])
	if err := errors.Join(err1, err2, err3); err != nil {
		return a, errors.New("invalid base64url")
	}
	var h struct {
		Alg string `json:"alg"`
		X5t string `json:"x5t"`
	}
	var p struct {
		Aud string  `json:"aud"`
		Iss string  `json:"iss"`
		Sub string  `json:"sub"`
		Nbf float64 `json:"nbf"`
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(hb, &h) != nil || json.Unmarshal(pb, &p) != nil {
		return a, errors.New("invalid JSON")
	}
	return assertion{
		Alg: h.Alg, X5t: h.X5t, Aud: p.Aud, Iss: p.Iss, Sub: p.Sub,
		Nbf: int64(p.Nbf), Exp: int64(p.Exp),
		signingInput: parts[0] + "." + parts[1], signature: sig,
	}, nil
}

func (a assertion) verify(pub *rsa.PublicKey) error {
	digest := sha256.Sum256([]byte(a.signingInput))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], a.signature)
}

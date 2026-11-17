package graph

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // x5t is defined as the SHA-1 thumbprint
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

// Credential kinds reported by Credential.Kind.
const (
	KindCertificate = "certificate"
	KindSecret      = "secret"
)

const assertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// assertionLifetime is how long a signed client assertion stays valid.
const assertionLifetime = 10 * time.Minute

// Credential proves the application's identity to Entra ID. The two
// implementations are *ClientCertificate (preferred) and *ClientSecret.
type Credential interface {
	// Kind is KindCertificate or KindSecret.
	Kind() string
	// authenticate adds the proof to a token request form and returns the
	// sensitive values it added, so they can be scrubbed from error text.
	authenticate(form url.Values, clientID, audience string, now time.Time) (sensitive []string, err error)
}

// ClientSecret is a client-secret credential. Its formatted output never
// contains the secret.
type ClientSecret struct {
	// value is a closure rather than a string so that no fmt verb, not even
	// on a struct that embeds a copy of this one, can reach the secret.
	value func() string
}

// NewClientSecret wraps an application client secret.
func NewClientSecret(secret string) (*ClientSecret, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, fail.New(
			"entra.secret_missing",
			"The Entra client secret is empty",
			"The environment variable or file the config file points at has no value.",
			"Set the secret (or, better, configure a certificate) and run `taut auth` again.",
		)
	}
	return &ClientSecret{value: func() string { return secret }}, nil
}

// Kind implements Credential.
func (ClientSecret) Kind() string { return KindSecret }

// Format implements fmt.Formatter so the secret cannot be printed by any verb.
func (ClientSecret) Format(f fmt.State, _ rune) {
	fmt.Fprint(f, "graph.ClientSecret{[redacted]}")
}

func (s ClientSecret) authenticate(form url.Values, _, _ string, _ time.Time) ([]string, error) {
	if s.value == nil {
		return nil, fail.New("entra.secret_missing", "The Entra client secret is empty",
			"A zero ClientSecret was used.", "Create it with NewClientSecret; this is a bug in Taut.")
	}
	form.Set("client_secret", s.value())
	return []string{s.value()}, nil
}

// ClientCertificate is a certificate credential: an X.509 certificate and its
// RSA private key. Its formatted output never contains key material.
type ClientCertificate struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	sha1 [sha1.Size]byte
}

// LoadClientCertificate reads a PEM file containing a certificate and its
// RSA private key.
func LoadClientCertificate(path string) (*ClientCertificate, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fail.Wrap(err,
			"entra.certificate_unreadable",
			"The Entra certificate file could not be read",
			"",
			"Check that the path in the config file exists and is readable by this user, then run `taut auth` again.",
		)
	}
	return ParseClientCertificate(data)
}

// ParseClientCertificate parses PEM data containing a certificate and an
// unencrypted RSA private key in PKCS#8 or PKCS#1 form. When several
// certificates are present, the one matching the key is used.
func ParseClientCertificate(pemData []byte) (*ClientCertificate, error) {
	const (
		whatBad  = "The Entra certificate file is not usable"
		nextPair = "Provide one PEM file holding both the certificate and its unencrypted RSA private key, " +
			"for example: `cat taut.crt taut.key > taut.pem`, then run `taut auth` again."
	)
	var (
		certs       []*x509.Certificate
		key         *rsa.PrivateKey
		encrypted   bool
		nonRSA      string
		parseErr    error
		sawAnyBlock bool
	)
	for rest := pemData; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		sawAnyBlock = true
		if strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") || block.Type == "ENCRYPTED PRIVATE KEY" {
			encrypted = true
			continue
		}
		switch block.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				parseErr = err
				continue
			}
			certs = append(certs, c)
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				parseErr = err
				continue
			}
			if rk, ok := k.(*rsa.PrivateKey); ok {
				key = rk
			} else {
				nonRSA = fmt.Sprintf("%T", k)
			}
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				parseErr = err
				continue
			}
			key = k
		case "EC PRIVATE KEY":
			nonRSA = "an elliptic-curve key"
		}
	}

	switch {
	case !sawAnyBlock:
		return nil, fail.New("entra.bad_certificate", whatBad,
			"The file contains no PEM blocks. A .pfx/.p12 or DER file cannot be read directly.",
			"Convert it to PEM, for example `openssl pkcs12 -in taut.pfx -out taut.pem -nodes`, then run `taut auth` again.")
	case key == nil && encrypted:
		return nil, fail.New("entra.key_encrypted", whatBad,
			"The private key is encrypted with a passphrase, which Taut does not support.",
			"Decrypt it into a file only Taut can read, for example "+
				"`openssl pkey -in taut.key -out taut-plain.key`, and reference that file instead.")
	case key == nil && nonRSA != "":
		return nil, fail.New("entra.key_unsupported", whatBad,
			"The private key is "+nonRSA+", but Entra certificate credentials signed by Taut must use RSA.",
			"Create an RSA key pair (2048 bits or more), upload its certificate to the app registration, and run `taut auth` again.")
	case key == nil && parseErr != nil:
		return nil, fail.Wrap(parseErr, "entra.bad_certificate", whatBad,
			"The private key could not be parsed: "+parseErr.Error()+".", nextPair)
	case key == nil:
		return nil, fail.New("entra.key_missing", whatBad,
			"The file has no private key block (\"PRIVATE KEY\" or \"RSA PRIVATE KEY\").", nextPair)
	case len(certs) == 0 && parseErr != nil:
		return nil, fail.Wrap(parseErr, "entra.bad_certificate", whatBad,
			"The certificate could not be parsed: "+parseErr.Error()+".", nextPair)
	case len(certs) == 0:
		return nil, fail.New("entra.certificate_missing", whatBad,
			"The file has a private key but no \"CERTIFICATE\" block.", nextPair)
	}

	for _, c := range certs {
		pub, ok := c.PublicKey.(*rsa.PublicKey)
		if ok && pub.Equal(&key.PublicKey) {
			return &ClientCertificate{cert: c, key: key, sha1: sha1.Sum(c.Raw)}, nil //nolint:gosec // thumbprint
		}
	}
	return nil, fail.New("entra.key_mismatch", whatBad,
		"The private key does not belong to any certificate in the file.",
		"Pair the certificate with the key it was created from, then run `taut auth` again.")
}

// Kind implements Credential.
func (*ClientCertificate) Kind() string { return KindCertificate }

// Thumbprint returns the certificate's SHA-1 thumbprint as upper-case hex,
// the form the Entra admin center displays.
func (c ClientCertificate) Thumbprint() string {
	return strings.ToUpper(hex.EncodeToString(c.sha1[:]))
}

// Subject returns the certificate's subject distinguished name.
func (c *ClientCertificate) Subject() string { return c.cert.Subject.String() }

// NotBefore returns the start of the certificate's validity period.
func (c *ClientCertificate) NotBefore() time.Time { return c.cert.NotBefore }

// NotAfter returns the end of the certificate's validity period.
func (c *ClientCertificate) NotAfter() time.Time { return c.cert.NotAfter }

// Format implements fmt.Formatter so key material cannot be printed.
func (c ClientCertificate) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "graph.ClientCertificate{thumbprint: %s}", c.Thumbprint())
}

func (c *ClientCertificate) authenticate(form url.Values, clientID, audience string, now time.Time) ([]string, error) {
	const next = "Create a new certificate, upload it to the app registration " +
		"(Certificates & secrets), point the config file at the new PEM file, and run `taut auth` again."
	if now.After(c.cert.NotAfter) {
		return nil, fail.New("entra.certificate_expired",
			"The Entra certificate has expired",
			fmt.Sprintf("Certificate %s expired on %s.", c.Thumbprint(), c.cert.NotAfter.UTC().Format(time.DateOnly)),
			next)
	}
	// Allow the same five minutes of clock skew Entra does.
	if now.Add(5 * time.Minute).Before(c.cert.NotBefore) {
		return nil, fail.New("entra.certificate_not_yet_valid",
			"The Entra certificate is not valid yet",
			fmt.Sprintf("Certificate %s is valid from %s; this machine's clock says %s.",
				c.Thumbprint(), c.cert.NotBefore.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339)),
			"Check this machine's clock. If it is right, wait until the certificate becomes valid or issue a new one.")
	}
	a, err := c.assertion(clientID, audience, now)
	if err != nil {
		return nil, err
	}
	form.Set("client_assertion_type", assertionType)
	form.Set("client_assertion", a)
	return []string{a}, nil
}

// assertion builds the signed client assertion described in Microsoft's
// "certificate credentials" specification.
func (c *ClientCertificate) assertion(clientID, audience string, now time.Time) (string, error) {
	header := struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		X5t string `json:"x5t"`
	}{"RS256", "JWT", b64(c.sha1[:])}
	claims := struct {
		Aud string `json:"aud"`
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Jti string `json:"jti"`
		Nbf int64  `json:"nbf"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}{audience, clientID, clientID, newGUID(), now.Unix(), now.Unix(), now.Add(assertionLifetime).Unix()}

	h, _ := json.Marshal(header)
	p, _ := json.Marshal(claims)
	signingInput := b64(h) + "." + b64(p)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fail.Wrap(err, "entra.bad_certificate",
			"The client assertion could not be signed",
			"The RSA private key rejected the signing operation: "+err.Error()+".",
			"Create a new RSA key pair (2048 bits or more), upload its certificate to the app registration, and run `taut auth` again.")
	}
	return signingInput + "." + b64(sig), nil
}

package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
)

// exampleYAML is the documented schema, fully populated and valid.
const exampleYAML = `apiVersion: taut.dev/v1alpha1
kind: Config
metadata:
  name: acme-migration
source:
  slack:
    export: ./slack-export.zip
    api:
      enabled: true
      token: { env: TAUT_SLACK_TOKEN }
target:
  entra:
    tenantId: 3f2504e0-4f89-11d3-9a0c-0305e82c3301
    clientId: 9b2e7c1a-52d4-4f0e-8a3b-6c1d2e3f4a5b
    cloud: global
    credential:
      certificate: { file: ./taut-app.pem }
  teams:
    - name: Acme
      description: Migrated from Slack
      owner: admin@acme.com
      visibility: private
      channels:
        include: ["*"]
        exclude: ["random"]
        types: [public]
        rename: { old-name: New Name }
  directMessages:
    enabled: false
identity:
  overrides: ./identity-overrides.csv
  unmapped:
    policy: former-member
    account: taut-former-member@acme.com
content:
  reactions: footer
  customEmoji: text
  since: 2020-01-01
  until: ""
load:
  strategy: modern
staging:
  endpoint: http://localhost:9000
  region: us-east-1
  bucket: taut-staging
  accessKey: { env: TAUT_S3_ACCESS_KEY }
  secretKey: { env: TAUT_S3_SECRET_KEY }
  encryptionKey: { env: TAUT_STAGING_KEY }
controller:
  url: http://localhost:7070
`

// Fake credentials, assembled at run time so the source holds no
// token-shaped literal.
var (
	fakeSlackToken = "xoxb-" + "2048151234-2048159876543-" + "AbCdEfGhIjKlMnOpQrStUvWx"
	fakeJWT        = "eyJ" + "hbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9" + "." +
		"eyJ" + "zdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ" + "." +
		"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	fakeEntraSecret = "abc8Q" + "~" + "dEfGhIjKlMnOpQrStUvWxYz0123456789_-"
	fakePEMBody     = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj"
	fakePEM         = "-----BEGIN " + "PRIVATE KEY-----\n" + fakePEMBody + "\n-----END " + "PRIVATE KEY-----"
)

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

func example(t *testing.T) *Config {
	t.Helper()
	return mustParse(t, exampleYAML)
}

// asFail asserts err is a well-formed *fail.Error and returns it.
func asFail(t *testing.T, err error) *fail.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var fe *fail.Error
	if !errors.As(err, &fe) {
		t.Fatalf("error is %T, want *fail.Error: %v", err, err)
	}
	if fe.Code == "" || fe.What == "" || fe.Next == "" {
		t.Errorf("error lacks Code, What or Next: %+v", fe)
	}
	return fe
}

// issuesOf extracts the structured issues from a "config.invalid" error.
func issuesOf(t *testing.T, err error) Issues {
	t.Helper()
	fe := asFail(t, err)
	if fe.Code != "config.invalid" {
		t.Fatalf("code = %q, want config.invalid (%v)", fe.Code, err)
	}
	var ie *InvalidError
	if !errors.As(err, &ie) {
		t.Fatalf("error does not carry *InvalidError: %v", err)
	}
	return ie.Issues
}

func find(is Issues, path string) (Issue, bool) {
	for _, i := range is {
		if i.Path == path {
			return i, true
		}
	}
	return Issue{}, false
}

func dump(is Issues) string {
	var b strings.Builder
	for _, i := range is {
		b.WriteString("\n  [" + string(i.Severity) + "] " + i.String())
	}
	return b.String()
}

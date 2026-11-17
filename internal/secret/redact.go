package secret

import (
	"regexp"
	"strings"
	"unicode"
)

// Value character classes shared by the patterns below.
const (
	// headerValue is a header value up to the next delimiter. The
	// alternation keeps an already-redacted value intact so that Redact is
	// idempotent.
	headerValue = `(?:\[redacted\]|[^\s"',;\]]+)`
	// paramValue is a query or form parameter value.
	paramValue = `[^&\s"'<>#]+`
)

var (
	// PEM private key blocks; an unterminated block (truncated output) is
	// scrubbed to the end of the text.
	rePEM = regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----.*?(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----|\z)`)
	// rePEMHeader detects the start of a private key block.
	rePEMHeader = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`)

	// "Authorization: Bearer <anything>", including quoted/JSON and Go
	// http.Header (map[Authorization:[Bearer …]]) spellings.
	reAuthScheme = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization["']?\s*[:=]\s*[\["']?(?:bearer|basic|digest|negotiate|token)\s+)` + headerValue)
	// "Authorization: <opaque>" with no scheme; only long values, so prose
	// such as "authorization: required" survives.
	reAuthBare = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization["']?\s*[:=]\s*[\["']?)[^\s"',;\]\[]{16,}`)
	// A bare "Bearer <token>". Filtered further by looksLikeBearerToken.
	reBearer = regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]{16,})`)

	// JSON Web Tokens: three or more base64url segments, the first a JSON
	// object ("eyJ" is base64 for `{"`).
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}(?:\.[A-Za-z0-9_-]+)*`)

	// Slack tokens: bot/user/app/legacy (xox?-), rotating (xoxe.xoxp-,
	// xoxe-) and app-level (xapp-).
	reSlack = regexp.MustCompile(`\b(?:(?:xoxe\.)?xox[abeprs]|xapp|xoxe)-[A-Za-z0-9-]{6,}`)

	// Entra client secrets: 3 characters, a digit, the "Q~" marker, then
	// 31 or more characters of the secret alphabet.
	reEntra = regexp.MustCompile(`[A-Za-z0-9_~.-]{3}[0-9]Q~[A-Za-z0-9_~.-]{31,}`)

	// JSON members such as "client_secret":"…".
	reJSONMember = regexp.MustCompile(`(?i)("(?:[a-z0-9_]*token|client_secret|client_assertion|password)"\s*:\s*")[^"]*`)
	// Query and form parameters such as token=…, access_token=…,
	// client_secret=…, client_assertion=…, sig=… .
	reParam = regexp.MustCompile(`(?i)\b((?:[a-z0-9_.-]*token|client_secret|client_assertion|password|x-amz-signature|sig)=)` + paramValue)
	// Slack's short-lived file download parameter, ?t=… / &t=… .
	reTParam = regexp.MustCompile(`([?&]t=)` + paramValue)
	// SharePoint's pre-authorization parameter on upload and download URLs.
	reTempAuth = regexp.MustCompile(`(?i)\b(tempauth=)` + paramValue)
	// A password in a URL's userinfo: scheme://user:password@host.
	reUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]*:)[^\s@/]+(@)`)
)

// Redact returns s with secret-looking material replaced by Marker. It
// recognizes Slack tokens, Authorization headers and bare Bearer tokens,
// JWTs, Entra client secrets, PEM private key blocks, and token-carrying
// query, form and JSON parameters (t, token, *_token, client_secret,
// client_assertion, password, sig). Text around a match is preserved and the
// function is idempotent: Redact(Redact(s)) == Redact(s).
func Redact(s string) string {
	if len(s) < 4 {
		return s
	}
	if strings.Contains(s, "PRIVATE KEY-----") {
		s = rePEM.ReplaceAllString(s, Marker)
	}
	s = reAuthScheme.ReplaceAllString(s, "${1}"+Marker)
	s = reAuthBare.ReplaceAllString(s, "${1}"+Marker)
	s = reBearer.ReplaceAllStringFunc(s, func(m string) string {
		sub := reBearer.FindStringSubmatch(m)
		if !looksLikeBearerToken(sub[2]) {
			return m
		}
		return sub[1] + Marker
	})
	s = reJWT.ReplaceAllString(s, Marker)
	s = reSlack.ReplaceAllString(s, Marker)
	s = reEntra.ReplaceAllString(s, Marker)
	s = reJSONMember.ReplaceAllString(s, "${1}"+Marker)
	s = reParam.ReplaceAllString(s, "${1}"+Marker)
	s = reTParam.ReplaceAllString(s, "${1}"+Marker)
	s = reTempAuth.ReplaceAllString(s, "${1}"+Marker)
	s = reUserinfo.ReplaceAllString(s, "${1}"+Marker+"${2}")
	return s
}

// looksLikeBearerToken separates credentials from prose that merely follows
// the word "Bearer" ("Bearer authentication is required").
func looksLikeBearerToken(tok string) bool {
	if len(tok) >= 32 {
		return true
	}
	return strings.ContainsFunc(tok, unicode.IsDigit)
}

// Kinds reported by LooksLikeSecret.
const (
	KindSlackToken  = "Slack token"
	KindJWT         = "JWT"
	KindPrivateKey  = "private key"
	KindEntraSecret = "Entra client secret" //nolint:gosec // a label, not a credential
)

// reSlackStrict is reSlack with a longer body, for detection.
var reSlackStrict = regexp.MustCompile(`\b(?:(?:xoxe\.)?xox[abeprs]|xapp|xoxe)-[A-Za-z0-9-]{10,}`)

// reFileExt matches a trailing file extension such as ".zip" or ".pem".
var reFileExt = regexp.MustCompile(`\.[A-Za-z]{2,4}$`)

// LooksLikeSecret reports whether s contains a literal credential and, if
// so, what kind. It is used to reject secrets written directly into the
// config file, and is deliberately conservative: names, GUIDs, URLs, e-mail
// addresses and file paths are not flagged.
func LooksLikeSecret(s string) (kind string, ok bool) {
	if len(s) < 16 {
		return "", false
	}
	if strings.Contains(s, "PRIVATE KEY-----") && rePEMHeader.MatchString(s) {
		return KindPrivateKey, true
	}
	for _, m := range reSlackStrict.FindAllString(s, -1) {
		// Real tokens carry numeric workspace and app IDs; requiring a
		// digit spares names such as "xoxb-is-a-prefix".
		if strings.ContainsFunc(m, unicode.IsDigit) {
			return KindSlackToken, true
		}
	}
	if reJWT.MatchString(s) {
		return KindJWT, true
	}
	if reEntra.MatchString(s) || looksLikeEntraSecret(strings.TrimSpace(s)) {
		return KindEntraSecret, true
	}
	return "", false
}

// looksLikeEntraSecret matches client secrets that lack the "Q~" marker
// (older ones): a single 32–44 character run of the secret alphabet that
// contains "~" and mixes upper case, lower case and digits. Anything ending
// in a file extension is taken to be a file name instead.
func looksLikeEntraSecret(s string) bool {
	if len(s) < 32 || len(s) > 44 || !strings.Contains(s, "~") || reFileExt.MatchString(s) {
		return false
	}
	var upper, lower, digit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digit = true
		case r == '_' || r == '~' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return upper && lower && digit
}

package secret

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"slack bot token", "token is " + fakeSlackBot + " ok", "token is [redacted] ok"},
		{"slack app token", "app=" + fakeSlackApp + ";", "app=[redacted];"},
		{"slack user token in parens", "(xoxp-" + "1234567890-abcdef)", "([redacted])"},
		{"slack rotating token", "xoxe.xoxp-" + "1-Mi0xLTEyMzQ1Ng next", "[redacted] next"},
		{"authorization bearer header", "Authorization: Bearer abc.def-123\nHost: slack.com", "Authorization: Bearer [redacted]\nHost: slack.com"},
		{"authorization basic header", "authorization: Basic dXNlcjpwYXNz", "authorization: Basic [redacted]"},
		{"authorization short bearer", "Authorization: Bearer abc", "Authorization: Bearer [redacted]"},
		{"authorization json", `{"Authorization":"Bearer abc123","x":1}`, `{"Authorization":"Bearer [redacted]","x":1}`},
		{"authorization go header map", "map[Authorization:[Bearer abc123def] Accept:[*/*]]", "map[Authorization:[Bearer [redacted]] Accept:[*/*]]"},
		{"authorization opaque", "Authorization: 0123456789abcdef0123", "Authorization: [redacted]"},
		{"bare bearer", "sent Bearer 0a1b2c3d4e5f6a7b8c9d to graph", "sent Bearer [redacted] to graph"},
		{"jwt", "assertion " + fakeJWT + " rejected", "assertion [redacted] rejected"},
		{"bearer jwt", "Bearer " + fakeJWT, "Bearer [redacted]"},
		{"slack file t param", "GET https://files.slack.com/files-pri/T1-F2/a.png?t=" + fakeSlackBot + " 200", "GET https://files.slack.com/files-pri/T1-F2/a.png?t=[redacted] 200"},
		{"t param second", "https://x.test/f?a=1&t=abc123&b=2", "https://x.test/f?a=1&t=[redacted]&b=2"},
		{"token param", "https://x.test/f?token=abc123&b=2", "https://x.test/f?token=[redacted]&b=2"},
		{"access_token param", "access_token=abc123&expires_in=3600", "access_token=[redacted]&expires_in=3600"},
		{"client_secret form", "grant_type=client_credentials&client_secret=p%40ss~word&scope=x", "grant_type=client_credentials&client_secret=[redacted]&scope=x"},
		{"client_assertion form", "client_assertion=" + fakeJWT + "&client_id=11111111-2222-3333-4444-555555555555", "client_assertion=[redacted]&client_id=11111111-2222-3333-4444-555555555555"},
		{"json client_secret", `{"client_secret":"hunter2","tenant":"acme"}`, `{"client_secret":"[redacted]","tenant":"acme"}`},
		{"json access_token", `{"token_type":"Bearer","access_token": "abc"}`, `{"token_type":"Bearer","access_token": "[redacted]"}`},
		{"entra secret", "secret " + fakeEntra + " expired", "secret [redacted] expired"},
		{"pem block", "key:\n" + fakePEM + "\nend", "key:\n[redacted]\nend"},
		{"rsa pem block", "-----BEGIN RSA " + "PRIVATE KEY-----\nAAAA\n-----END RSA " + "PRIVATE KEY-----", "[redacted]"},
		{"truncated pem block", "x -----BEGIN EC " + "PRIVATE KEY-----\nAAAA\nBBBB", "x [redacted]"},
		{"multiple", fakeSlackBot + " and " + fakeJWT, "[redacted] and [redacted]"},

		// Negatives: ordinary text must survive untouched.
		{"empty", "", ""},
		{"prose", "Migrating 14 channels from Acme to Teams took 3m20s.", "Migrating 14 channels from Acme to Teams took 3m20s."},
		{"uuid", "tenant 3f2504e0-4f89-11d3-9a0c-0305e82c3301 not found", "tenant 3f2504e0-4f89-11d3-9a0c-0305e82c3301 not found"},
		{"unix path", "open /Users/alex/dev/taut/slack-export.zip: no such file", "open /Users/alex/dev/taut/slack-export.zip: no such file"},
		{"home path", "~/.config/taut/taut-app.pem", "~/.config/taut/taut-app.pem"},
		{"windows path", `C:\Users\alex\tokens\notes.txt`, `C:\Users\alex\tokens\notes.txt`},
		{"url without secrets", "https://graph.microsoft.com/v1.0/teams?$top=10&filter=x", "https://graph.microsoft.com/v1.0/teams?$top=10&filter=x"},
		{"bearer prose", "Bearer authentication is required by Graph", "Bearer authentication is required by Graph"},
		{"authorization prose", "authorization: required", "authorization: required"},
		{"token prose", "the token expired; set token=<your token> and retry", "the token expired; set token=<your token> and retry"},
		{"t-like params", "?at=1&start=2&cat=3", "?at=1&start=2&cat=3"},
		{"public key pem", "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----", "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----"},
		{"certificate pem", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"},
		{"env reference", "token: { env: TAUT_SLACK_TOKEN }", "token: { env: TAUT_SLACK_TOKEN }"},
		{"email", "owner admin@acme.com", "owner admin@acme.com"},
		{"sha", "commit 9fceb02d0ae598e95dc970b74767f19372d61af8", "commit 9fceb02d0ae598e95dc970b74767f19372d61af8"},
		{"channel named like a prefix", "channel xoxo-team", "channel xoxo-team"},
		{"marker", "already [redacted] here", "already [redacted] here"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if got != tt.want {
				t.Errorf("Redact(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
			if again := Redact(got); again != got {
				t.Errorf("not idempotent:\n once  %q\n twice %q", got, again)
			}
		})
	}
}

func TestRedactRemovesEveryFixture(t *testing.T) {
	all := strings.Join([]string{fakeSlackBot, fakeSlackApp, fakeJWT, fakeEntra, fakePEM}, "\n")
	got := Redact("before\n" + all + "\nafter")
	for _, s := range []string{fakeSlackBot, fakeSlackApp, fakeJWT, fakeEntra, "MIIEvQIBADANBgkq"} {
		if strings.Contains(got, s) {
			t.Errorf("fixture survived redaction: %q", got)
		}
	}
	if !strings.HasPrefix(got, "before\n") || !strings.HasSuffix(got, "\nafter") {
		t.Errorf("surrounding text was damaged: %q", got)
	}
}

func TestLooksLikeSecret(t *testing.T) {
	tests := []struct {
		name string
		in   string
		kind string
	}{
		{"slack bot", fakeSlackBot, KindSlackToken},
		{"slack app", fakeSlackApp, KindSlackToken},
		{"slack embedded", "Bearer " + fakeSlackBot, KindSlackToken},
		{"jwt", fakeJWT, KindJWT},
		{"pem", fakePEM, KindPrivateKey},
		{"pem with leading text", "key: |\n  " + fakePEM, KindPrivateKey},
		{"entra with marker", fakeEntra, KindEntraSecret},
		{"entra embedded", "secret=" + fakeEntra, KindEntraSecret},
		{"entra legacy shape", "7fK~w.Tq3-Zr8_Lm2Np5Xb9Vc1Hd4Jg6Sa0", KindEntraSecret},

		{"empty", "", ""},
		{"name", "acme-migration", ""},
		{"display name", "Acme Engineering ~ Platform Team 2024", ""},
		{"guid", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", ""},
		{"upn", "taut-former-member@acme.onmicrosoft.com", ""},
		{"url", "https://login.microsoftonline.com/3f2504e0-4f89-11d3-9a0c-0305e82c3301/v2.0", ""},
		{"localhost url", "http://localhost:9000", ""},
		{"relative path", "./exports/2024/slack-export-Acme-Corp.zip", ""},
		{"home path", "~/Secrets/Taut/Acme-2024-Migration/app.pem", ""},
		{"long home-ish file name", "~Backup-Of-Slack-Export-2024-Final.zip", ""},
		{"env name", "TAUT_ENTRA_CLIENT_SECRET", ""},
		{"date", "2020-01-01", ""},
		{"glob", "proj-*-2024", ""},
		{"slack-like channel", "xoxb-fans-club-channel", ""},
		{"sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", ""},
		{"public key", "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----", ""},
		{"sentence", "Migrated from Slack by Taut on behalf of the IT team.", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, ok := LooksLikeSecret(tt.in)
			if kind != tt.kind || ok != (tt.kind != "") {
				t.Errorf("LooksLikeSecret = (%q, %v), want (%q, %v)", kind, ok, tt.kind, tt.kind != "")
			}
		})
	}
}

func TestRedactURLCredentials(t *testing.T) {
	cases := map[string]string{
		"postgres://taut:hunter2@db.internal:5432/taut":                   "hunter2",
		"connecting to nats://svc:s3cr3t-pw@nats:4222 failed":             "s3cr3t-pw",
		"PUT https://x.sharepoint.com/_api/upload?guid=1&tempauth=v1.abc": "v1.abc",
	}
	for in, secretPart := range cases {
		got := Redact(in)
		if strings.Contains(got, secretPart) {
			t.Errorf("Redact(%q) = %q, still contains %q", in, got, secretPart)
		}
		if Redact(got) != got {
			t.Errorf("Redact is not idempotent on %q", in)
		}
	}
	// URLs without a password, and ordinary text with colons, are untouched.
	for _, in := range []string{"https://graph.microsoft.com/v1.0/users", "postgres://db.internal:5432/taut", "time 10:30:45 at host:port"} {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

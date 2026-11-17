package secret

import "strings"

// Fake credentials, assembled at run time so that the source file itself
// never contains a token-shaped literal for external scanners to trip over.
var (
	fakeSlackBot = "xoxb-" + "2048151234-2048159876543-" + "AbCdEfGhIjKlMnOpQrStUvWx"
	fakeSlackApp = "xapp-" + "1-A0123ABCDEF-1234567890123-" + strings.Repeat("ab12", 8)
	fakeJWT      = "eyJ" + "hbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9" + "." +
		"eyJ" + "zdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ" + "." +
		"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	fakeEntra = "abc8Q" + "~" + "dEfGhIjKlMnOpQrStUvWxYz0123456789_-"
	fakePEM   = "-----BEGIN " + "PRIVATE KEY-----\n" +
		"MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKj\n" +
		"MzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDuictGeurT8jNbvJZHtCSuYEvu\n" +
		"-----END " + "PRIVATE KEY-----"
)

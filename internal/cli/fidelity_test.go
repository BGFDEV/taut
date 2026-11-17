package cli_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/testutil"
)

// edit rewrites the config file, failing the test if nothing changed.
func (e *env) edit(old, new string) {
	e.t.Helper()
	raw, err := os.ReadFile(e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	changed := strings.Replace(string(raw), old, new, 1)
	if changed == string(raw) {
		e.t.Fatalf("the config file does not contain %q:\n%s", old, raw)
	}
	if err := os.WriteFile(e.config, []byte(changed), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// With content.reactions: native the preflight says what becomes a Teams
// reaction and what does not, and that none of it has met a real tenant.
func TestPreflightNativeReactions(t *testing.T) {
	e := newEnv(t, "medium")
	e.init()
	e.edit("reactions: footer", "reactions: native")

	out, errOut, code := e.run("preflight")
	if code != fail.ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	noSecrets(t, out, errOut)
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"Imported as Teams reactions from the people who have a Microsoft 365 account, up to 250 a message",
		"Not verified on a real tenant",
		"Reactions imported as Teams reactions",
		"Reactions that cannot be Teams reactions",
		"This has not been run against a real tenant",
		"set content.reactions to footer",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	testutil.Golden(t, "preflight_native_plain", out)

	out, errOut, code = e.run("preflight", "--json")
	if code != fail.ExitOK || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var rep struct {
		Notes   []struct{ Code string }
		Content []struct {
			Key   string
			Count int
		}
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("preflight --json is not one JSON document: %v", err)
	}
	counts := map[string]int{}
	for _, c := range rep.Content {
		counts[c.Key] = c.Count
	}
	if counts["reactions"] == 0 || counts["reactions_native"] == 0 || counts["reactions_summarized"] == 0 {
		t.Errorf("content counts: %v", counts)
	}
	noted := false
	for _, n := range rep.Notes {
		noted = noted || n.Code == "content.reactions_native"
	}
	if !noted {
		t.Errorf("no content.reactions_native note: %+v", rep.Notes)
	}
}

// content.customEmoji: image was accepted and did nothing. It is refused
// now, with the reason and the value to use.
func TestCustomEmojiImageIsRefused(t *testing.T) {
	e := newEnv(t, "medium")
	e.init()
	e.edit("customEmoji: text", "customEmoji: image")
	out, errOut, code := e.run("preflight", "--offline")
	all := strings.Join(strings.Fields(out+errOut), " ")
	if code == fail.ExitOK {
		t.Fatalf("a config file with customEmoji: image was accepted:\n%s", all)
	}
	for _, want := range []string{"content.customEmoji", "cannot be image", "a Slack export holds no emoji images", "use text"} {
		if !strings.Contains(all, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, all)
		}
	}
}

// The matchers that guess are named in the config file, and with nobody to
// guess about the report shows that they matched nobody.
func TestPreflightWithMatchersOn(t *testing.T) {
	e := newEnv(t, "medium")
	e.init()
	e.edit("identity:\n", "identity:\n  match:\n    emailLocalPart: true\n    displayName: true\n")

	out, errOut, code := e.run("preflight")
	if code != fail.ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{"Matched by email name part 0", "Matched by display name 0"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(flat, "review these") || strings.Contains(flat, "identity.weak_matches") {
		t.Errorf("guesses are listed although the fixture's directory matches everyone by address:\n%s", out)
	}

	out, _, code = e.run("preflight", "--json")
	if code != fail.ExitOK {
		t.Fatalf("exit %d", code)
	}
	var rep struct {
		Identity struct {
			Matchers []string
			Matched  []struct {
				Method string
				Weak   bool
			}
			MappedByEmail int
		}
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Identity.Matchers, ",") != "email_local_part,display_name" || len(rep.Identity.Matched) != rep.Identity.MappedByEmail || len(rep.Identity.Matched) == 0 {
		t.Errorf("identity: %d matched, %d by email, matchers %v", len(rep.Identity.Matched), rep.Identity.MappedByEmail, rep.Identity.Matchers)
	}
	for _, m := range rep.Identity.Matched {
		if m.Method != "email" || m.Weak {
			t.Errorf("matched entry: %+v", m)
		}
	}

	// A misspelled matcher is an unknown field, not a silent no-op.
	e.edit("emailLocalPart: true", "emailLocalpart: true")
	out, errOut, code = e.run("preflight", "--offline")
	if code == fail.ExitOK || !strings.Contains(out+errOut, "emailLocalpart") {
		t.Errorf("a misspelled matcher: exit %d\n%s\n%s", code, out, errOut)
	}
}

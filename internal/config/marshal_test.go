package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
)

func roundTrip(t *testing.T, m *Config) *Config {
	t.Helper()
	data, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal()): %v\n%s", err, data)
	}
	return back
}

func TestMarshalRoundTrip(t *testing.T) {
	full := example(t)
	full.Content.Until = "2024-12-31"
	full.Source.Slack.API.BaseURL = "http://127.0.0.1:8081/api"
	full.Target.Entra.Authority = "http://127.0.0.1:8082"
	full.Target.Entra.GraphBaseURL = "http://127.0.0.1:8083/v1.0"
	full.Target.DirectMessages.Enabled = true
	second := DefaultTeam()
	second.Name, second.Owner, second.Visibility = "Engineering: Core #1", "eng.lead@acme.com", VisibilityPublic
	second.Description = "Multi-line\ndescription with: colons, 'quotes' and # hashes"
	second.Channels = ChannelSelection{
		Include: []string{"eng-*", "[a-c]?", "true", "123"},
		Types:   []string{ChannelPublic, ChannelPrivate},
		Rename:  map[string]string{"eng-general": "General", "no": "yes", "eng-ops": "Ops: 24/7"},
	}
	full.Target.Teams = append(full.Target.Teams, second)

	secretCred := example(t)
	secretCred.Target.Entra.Credential = Credential{ClientSecret: secret.Ref{Env: "TAUT_ENTRA_CLIENT_SECRET"}}
	secretCred.Identity = Identity{Unmapped: Unmapped{Policy: PolicyBlock}}
	secretCred.Content.Since = ""

	matchers := example(t)
	matchers.Identity.Match = Match{EmailLocalPart: true, DisplayName: true}
	oneMatcher := example(t)
	oneMatcher.Identity.Match.DisplayName = true

	cases := map[string]*Config{
		"example":       example(t),
		"default":       Default(),
		"full":          full,
		"client secret": secretCred,
		"matchers":      matchers,
		"one matcher":   oneMatcher,
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			back := roundTrip(t, m)

			a, b := *m, *back
			a.src, b.src = nil, nil
			if !reflect.DeepEqual(a, b) {
				t.Errorf("round trip changed the config file:\n before %+v\n after  %+v", a, b)
			}
			if !m.Equal(back) || !back.Equal(m) {
				t.Error("Equal() = false after a round trip")
			}

			first, _ := m.Marshal()
			second, _ := back.Marshal()
			if string(first) != string(second) {
				t.Errorf("Marshal is not stable:\n%s\n---\n%s", first, second)
			}
		})
	}

	changed := example(t)
	changed.Target.Teams[0].Channels.Rename["old-name"] = "Other"
	if example(t).Equal(changed) {
		t.Error("Equal() = true for different plans")
	}
}

func TestMarshalFormat(t *testing.T) {
	data, err := example(t).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)

	for _, want := range []string{
		"# Taut config file",
		"contains no secrets",
		"`taut init`",
		"\n\n# Where the data comes from",
		"\n\n# Where it goes",
		"\n\n# How Slack users map",
		"\n\n# Object store for staged data",
		"\n\n# The controller",
		"\napiVersion: taut.dev/v1alpha1\nkind: Config\nmetadata:\n  name: acme-migration\n",
		"      token: {env: TAUT_SLACK_TOKEN}\n",
		"      certificate: {file: ./taut-app.pem}\n",
		"  teams:\n    - name: Acme\n",
		"        exclude: [random]\n",
		"        types: [public]\n",
		"          old-name: New Name\n",
		"  since: 2020-01-01\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Empty test-override and optional fields are omitted.
	for _, absent := range []string{"baseURL", "authority", "graphBaseURL", "clientSecret", "until", "match:", "emailLocalPart", "displayName", "\t"} {
		if strings.Contains(out, absent) {
			t.Errorf("output should not contain %q:\n%s", absent, out)
		}
	}
	if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
		t.Error("output must end with exactly one newline")
	}
	for i, line := range strings.Split(out, "\n") {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("line %d has trailing whitespace: %q", i+1, line)
		}
		if indent := len(line) - len(strings.TrimLeft(line, " ")); indent%2 != 0 {
			t.Errorf("line %d is not indented by a multiple of two: %q", i+1, line)
		}
	}

	withOverrides := example(t)
	withOverrides.Source.Slack.API.BaseURL = "http://127.0.0.1:1"
	data, _ = withOverrides.Marshal()
	if !strings.Contains(string(data), "baseURL: http://127.0.0.1:1") {
		t.Errorf("a set override must be written:\n%s", data)
	}

	// The matchers that guess are written only when they are on, and only
	// the ones that are.
	withMatcher := example(t)
	withMatcher.Identity.Match.EmailLocalPart = true
	data, _ = withMatcher.Marshal()
	if !strings.Contains(string(data), "  match:\n    emailLocalPart: true\n") || strings.Contains(string(data), "displayName") {
		t.Errorf("identity.match is not written as set:\n%s", data)
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultPath)
	m := example(t)

	noStrays := func() {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != DefaultPath {
				t.Errorf("stray file left behind: %s", e.Name())
			}
		}
	}

	if err := m.Write(path, false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	noStrays()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", info.Mode().Perm())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Write: %v", err)
	}
	if !loaded.Equal(m) {
		t.Error("written config file does not load back equal")
	}

	// Refuses to overwrite, leaving the original intact.
	other := example(t)
	other.Metadata.Name = "replacement"
	err = other.Write(path, false)
	fe := asFail(t, err)
	if fe.Code != "config.exists" || fail.ExitCode(err) != fail.ExitUsage || !strings.Contains(fe.What, path) {
		t.Errorf("got %q exit %d: %v", fe.Code, fail.ExitCode(err), err)
	}
	noStrays()
	if still, _ := Load(path); still == nil || still.Metadata.Name != "acme-migration" {
		t.Error("a refused write modified the existing file")
	}

	// Overwrites when told to, and tightens a loose mode.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := other.Write(path, true); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	noStrays()
	if now, _ := Load(path); now == nil || now.Metadata.Name != "replacement" {
		t.Error("overwrite did not replace the file")
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode after overwrite = %04o, want 0600", info.Mode().Perm())
	}

	// overwrite=true also creates a file that does not exist yet.
	fresh := filepath.Join(dir, "sub")
	if err := os.Mkdir(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Write(filepath.Join(fresh, "new.yaml"), true); err != nil {
		t.Errorf("overwrite=true on a new path: %v", err)
	}

	// A missing directory is a structured error.
	err = m.Write(filepath.Join(dir, "no-such-dir", "taut.yaml"), false)
	if fe := asFail(t, err); fe.Code != "config.write_failed" || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("got %q: %v", fe.Code, err)
	}
}

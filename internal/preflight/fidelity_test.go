package preflight_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/identity"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/transform"
)

func withMatchers(m *config.Config) {
	m.Identity.Match = config.Match{EmailLocalPart: true, DisplayName: true}
}

func openFixture(t *testing.T, fixture string) *export.Export {
	t.Helper()
	path, err := fixtures.Write(fixture, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ex, err := export.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	return ex
}

func mustUsers(t *testing.T, fixture string) []slack.User {
	t.Helper()
	users, err := openFixture(t, fixture).Users()
	if err != nil {
		t.Fatal(err)
	}
	return users
}

// publicMessages returns the readable messages of a fixture's public
// channels, which are what the default config selects.
func publicMessages(t *testing.T, fixture string) []slack.Message {
	t.Helper()
	ex := openFixture(t, fixture)
	channels, err := ex.Channels()
	if err != nil {
		t.Fatal(err)
	}
	var out []slack.Message
	for _, ch := range channels {
		if ch.Kind != slack.KindPublic {
			continue
		}
		for msg, err := range ex.Messages(ch) {
			if err == nil {
				out = append(out, msg)
			}
		}
	}
	return out
}

// movedDirectory is the fixture's directory after a change of domain: the
// first few people kept the name part of their address, the next few got a
// new address and kept only their display name. It returns who is which.
func movedDirectory(fixture string) (dir *identity.Directory, byLocalPart, byName map[string]bool) {
	users := []identity.DirUser{
		{ID: "owner", UPN: owner, Mail: owner, Enabled: true},
		{ID: "former", UPN: former, Mail: former, Enabled: true},
	}
	byLocalPart, byName = map[string]bool{}, map[string]bool{}
	n := 0
	for _, p := range fixtures.People(fixture) {
		if !p.InDirectory || p.Email == "" {
			continue
		}
		mail := strings.ToLower(p.Email)
		local, _, _ := strings.Cut(mail, "@")
		switch n++; {
		case n <= 4:
			mail = local + "@new-domain.example"
			byLocalPart[p.SlackID] = true
		case n <= 8:
			mail = "employee-" + strings.ToLower(p.SlackID) + "@new-domain.example"
			byName[p.SlackID] = true
		}
		users = append(users, identity.DirUser{ID: "id-" + p.SlackID, DisplayName: p.DisplayName, Mail: mail, UPN: mail, Enabled: true})
	}
	return identity.NewDirectory(users), byLocalPart, byName
}

// The weak matchers are off unless asked for. When on, each match is listed
// with how it was made, and the report warns that they are guesses.
func TestWeakMatchesAreListedForReview(t *testing.T) {
	dir, byLocalPart, byName := movedDirectory("medium")
	if len(byLocalPart) != 4 || len(byName) != 4 {
		t.Fatalf("the fixture has too few people in the directory: %d and %d", len(byLocalPart), len(byName))
	}

	off := setup(t, "medium", nil)
	off.Directory = dir
	r := run(t, off)
	if r.Identity.MappedByEmailLocalPart != 0 || r.Identity.MappedByDisplayName != 0 || len(r.Identity.Matchers) != 0 ||
		strings.Contains(codes(r.Warnings), "identity.weak_matches") {
		t.Fatalf("matchers off: %+v, warnings %s", r.Identity, codes(r.Warnings))
	}
	unmappedOff := r.Identity.Unmapped
	for _, m := range r.Identity.Matched {
		if m.Weak || m.Method != string(identity.MethodEmail) || byLocalPart[m.SlackID] || byName[m.SlackID] {
			t.Errorf("matchers off: %+v", m)
		}
	}
	if len(r.Identity.Matched) != r.Identity.MappedByEmail {
		t.Errorf("%d people listed as matched, %d counted", len(r.Identity.Matched), r.Identity.MappedByEmail)
	}

	on := setup(t, "medium", withMatchers)
	on.Directory = dir
	r = run(t, on)
	id := r.Identity
	if id.MappedByEmailLocalPart != 4 || id.MappedByDisplayName != 4 || id.Unmapped != unmappedOff-8 {
		t.Fatalf("matchers on: %d by local part, %d by name, %d unmapped (was %d)", id.MappedByEmailLocalPart, id.MappedByDisplayName, id.Unmapped, unmappedOff)
	}
	if strings.Join(id.Matchers, ",") != "email_local_part,display_name" {
		t.Errorf("matchers = %v", id.Matchers)
	}
	if len(id.Matched) != id.MappedByEmail+id.MappedByOverride+8 {
		t.Errorf("%d people listed as matched", len(id.Matched))
	}
	// The guesses come first, each matched to the person's own account
	// and saying what it rests on.
	for i, m := range id.Matched {
		weak := byLocalPart[m.SlackID] || byName[m.SlackID]
		if m.Weak != weak || (i < 8) != weak {
			t.Errorf("entry %d (%s): weak=%t, want %t, and the weak ones first", i, m.SlackID, m.Weak, weak)
		}
		if m.EntraID != "id-"+m.SlackID || m.EntraUPN == "" || m.Detail == "" {
			t.Errorf("%s is matched to %s (%s)", m.SlackID, m.EntraID, m.EntraUPN)
		}
		switch {
		case byLocalPart[m.SlackID]:
			if local, _, _ := strings.Cut(strings.ToLower(m.Email), "@"); m.Method != "email_local_part" || m.MatchedOn != local {
				t.Errorf("%s: method %s on %q", m.SlackID, m.Method, m.MatchedOn)
			}
		case byName[m.SlackID]:
			if m.Method != "display_name" || m.MatchedOn != strings.ToLower(m.Name) {
				t.Errorf("%s: method %s on %q, name %q", m.SlackID, m.Method, m.MatchedOn, m.Name)
			}
		default:
			if m.Method != "email" || m.MatchedOn != "" {
				t.Errorf("%s: method %s on %q", m.SlackID, m.Method, m.MatchedOn)
			}
		}
	}
	var warning *preflight.Finding
	for i, w := range r.Warnings {
		if w.Code == "identity.weak_matches" {
			warning = &r.Warnings[i]
		}
	}
	if warning == nil {
		t.Fatalf("no warning about the guesses: %s", codes(r.Warnings))
	}
	for _, want := range []string{"8 people were matched", "(4)", "attributes one person's messages", "adds that other person to the team"} {
		if !strings.Contains(warning.Message, want) {
			t.Errorf("the warning does not say %q: %s", want, warning.Message)
		}
	}
	for _, want := range []string{"identity.overrides", `"none"`, "identity.match"} {
		if !strings.Contains(warning.Next, want) {
			t.Errorf("the next step does not name %s: %s", want, warning.Next)
		}
	}
	if r.Verdict != preflight.VerdictWarnings {
		t.Errorf("verdict = %s", r.Verdict)
	}

	// The JSON form carries the same list.
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Identity struct {
			MappedByEmailLocalPart int `json:"mappedByEmailLocalPart"`
			MappedByDisplayName    int `json:"mappedByDisplayName"`
			Matched                []struct {
				Method string `json:"method"`
				Weak   bool   `json:"weak"`
				UPN    string `json:"entraUpn"`
			} `json:"matched"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Identity.MappedByEmailLocalPart != 4 || doc.Identity.MappedByDisplayName != 4 || len(doc.Identity.Matched) != len(id.Matched) ||
		!doc.Identity.Matched[0].Weak || doc.Identity.Matched[0].UPN == "" {
		t.Errorf("JSON identity: %+v", doc.Identity)
	}

	// An operator rejects one guess and confirms another in the override
	// CSV. Neither is a guess any more.
	var reject, confirm string
	for id := range byLocalPart {
		if reject == "" {
			reject = id
		} else if confirm == "" {
			confirm = id
		}
	}
	on.Overrides = []identity.Override{{SlackID: reject, None: true}, {SlackID: confirm, Entra: "id-" + confirm}}
	r = run(t, on)
	if r.Identity.MappedByEmailLocalPart != 2 || r.Identity.MappedByOverride != 1 {
		t.Errorf("after overrides: %d by local part, %d by override", r.Identity.MappedByEmailLocalPart, r.Identity.MappedByOverride)
	}
	found := false
	for _, u := range r.Identity.UnmappedUsers {
		if u.SlackID == reject {
			found = true
			if u.Reason != string(identity.ReasonExcluded) || !strings.Contains(u.Detail, "override CSV") {
				t.Errorf("the rejected person: %+v", u)
			}
		}
	}
	if !found {
		t.Error("the rejected person is not listed as unmapped")
	}
}

// A name that fits more than one account, or more than one Slack user,
// matches nobody, and the report says why.
func TestWeakMatchersNeverGuessBetweenCandidates(t *testing.T) {
	o := setup(t, "small", withMatchers)
	people := fixtures.People("small")
	var first, second fixtures.Person
	for _, p := range people {
		if p.InDirectory && p.Email != "" {
			if first.SlackID == "" {
				first = p
			} else if second.SlackID == "" {
				second = p
			}
		}
	}
	local, _, _ := strings.Cut(strings.ToLower(first.Email), "@")
	users := []identity.DirUser{
		{ID: "owner", UPN: owner, Mail: owner, Enabled: true},
		{ID: "former", UPN: former, Mail: former, Enabled: true},
		// Two accounts answer to the first person's local part, and two
		// to the second person's name.
		{ID: "a1", DisplayName: "Somebody", Mail: local + "@one.example", UPN: local + "@one.example", Enabled: true},
		{ID: "a2", DisplayName: "Somebody Else", Mail: local + "@two.example", UPN: local + "@two.example", Enabled: true},
		{ID: "b1", DisplayName: second.DisplayName, Mail: "b1@one.example", UPN: "b1@one.example", Enabled: true},
		{ID: "b2", DisplayName: strings.ToUpper(second.DisplayName), Mail: "b2@one.example", UPN: "b2@one.example", Enabled: true},
	}
	o.Directory = identity.NewDirectory(users)
	r := run(t, o)
	if n := r.Identity.MappedByEmailLocalPart + r.Identity.MappedByDisplayName; n != 0 {
		t.Fatalf("%d people were matched between two candidates: %+v", n, r.Identity.Matched)
	}
	said := 0
	for _, u := range r.Identity.UnmappedUsers {
		if (u.SlackID == first.SlackID || u.SlackID == second.SlackID) && strings.Contains(u.Detail, "their name fits several accounts") {
			said++
		}
	}
	if said != 2 {
		t.Errorf("%d of the two undecided people say why: %+v", said, r.Identity.UnmappedUsers)
	}
	if strings.Contains(codes(r.Warnings), "identity.weak_matches") {
		t.Error("a warning about guesses although nobody was matched by one")
	}
}

// A guess puts a name on a message. It does not open a private
// conversation to someone.
func TestDirectMessagesNeedMoreThanAGuess(t *testing.T) {
	mk := func(id, email, name string) slack.User {
		u := slack.User{ID: id, RealName: name}
		u.Profile.Email = email
		return u
	}
	users := []slack.User{
		mk("UA", "ua@fixture.example", "Ann Exact"),
		mk("UB", "ub@fixture.example", "Bob Exact"),
		mk("UL", "lee@old.example", "Lee Local"),
		mk("UN", "nobody@old.example", "Nat Name"),
	}
	dir := identity.NewDirectory([]identity.DirUser{
		{ID: "id-UA", Mail: "ua@fixture.example", UPN: "ua@fixture.example", DisplayName: "Ann Exact", Enabled: true},
		{ID: "id-UB", Mail: "ub@fixture.example", UPN: "ub@fixture.example", DisplayName: "Bob Exact", Enabled: true},
		{ID: "id-UL", Mail: "lee@fixture.example", UPN: "lee@fixture.example", DisplayName: "L. Local", Enabled: true},
		{ID: "id-UN", Mail: "nn@fixture.example", UPN: "nn@fixture.example", DisplayName: "Nat Name", Enabled: true},
	})
	channels := []slack.Channel{
		{ID: "D1", Kind: slack.KindDM, Members: []string{"UA", "UB"}},
		{ID: "D2", Kind: slack.KindDM, Members: []string{"UA", "UL"}},
		{ID: "D3", Kind: slack.KindMPIM, Members: []string{"UA", "UB", "UN"}},
	}
	idmap := identity.Resolve(users, dir, nil, identity.Options{EmailLocalPart: true, DisplayName: true})
	if !idmap.Lookup("UL").Mapped() || !idmap.Lookup("UN").Mapped() {
		t.Fatal("the guesses were not made")
	}
	got := preflight.DirectMessages(channels, users, idmap, true)
	want := []string{"", preflight.DMSkipAccountGuessed, preflight.DMSkipAccountGuessed}
	for i, d := range got {
		if d.Skip != want[i] {
			t.Errorf("%s: skip %q, want %q", d.SlackID, d.Skip, want[i])
		}
	}
	if reason := preflight.DMSkipReason(preflight.DMSkipAccountGuessed); !strings.Contains(reason, "identity.overrides") {
		t.Errorf("the reason does not say how to confirm the match: %s", reason)
	}
	// Confirmed in the override CSV, the same people are no guess.
	idmap = identity.Resolve(users, dir, []identity.Override{{SlackID: "UL", Entra: "id-UL"}, {SlackID: "UN", Entra: "id-UN"}}, identity.Options{EmailLocalPart: true, DisplayName: true})
	for _, d := range preflight.DirectMessages(channels, users, idmap, true) {
		if d.Skip != "" {
			t.Errorf("%s: skip %q after the matches were confirmed", d.SlackID, d.Skip)
		}
	}
}

// What the Content section says about reactions follows the setting, and
// under native it counts what will and will not be a Teams reaction.
func TestReactionsContentByMode(t *testing.T) {
	handling := func(r *preflight.Report, key string) string {
		for _, c := range r.Content {
			if c.Key == key {
				return c.Handling
			}
		}
		return ""
	}
	footer := run(t, setup(t, "nasty", nil))
	if h := handling(footer, "reactions"); !strings.HasPrefix(h, "Summarized in a line under the message") {
		t.Errorf("footer: %q", h)
	}
	if item(footer, "reactions_native") != 0 || item(footer, "reactions_summarized") != 0 || strings.Contains(codes(footer.Notes), "content.reactions_native") {
		t.Errorf("footer counts native reactions: %+v, notes %s", footer.Content, codes(footer.Notes))
	}

	drop := run(t, setup(t, "nasty", func(m *config.Config) { m.Content.Reactions = config.ReactionsDrop }))
	if h := handling(drop, "reactions"); !strings.HasPrefix(h, "Dropped") || item(drop, "reactions_native") != 0 {
		t.Errorf("drop: %q", h)
	}
	for _, c := range drop.Content {
		if c.Key == "reactions" && !c.Lossy {
			t.Error("dropping reactions is not marked as a loss")
		}
	}

	native := run(t, setup(t, "nasty", func(m *config.Config) { m.Content.Reactions = config.ReactionsNative }))
	h := handling(native, "reactions")
	for _, want := range []string{"Imported as Teams reactions", "up to 250 a message", "Not verified on a real tenant"} {
		if !strings.Contains(h, want) {
			t.Errorf("native: the handling does not say %q: %s", want, h)
		}
	}
	if item(native, "reactions") != item(footer, "reactions") {
		t.Errorf("messages with reactions: %d under native, %d under footer", item(native, "reactions"), item(footer, "reactions"))
	}
	// Count them the way the transform divides them, from the export.
	wantNative, wantRest := 0, 0
	idmap := identity.Resolve(mustUsers(t, "nasty"), directoryFor("nasty"), nil, identity.Options{})
	for _, msg := range publicMessages(t, "nasty") {
		if transform.Classify(msg).Disposition != transform.Import {
			continue
		}
		for _, re := range msg.Reactions {
			seen := map[string]bool{}
			n := 0
			for _, u := range re.Users {
				if e := idmap.Lookup(u); e != nil && e.Mapped() && !seen[e.EntraID] && re.Name != "partyparrot" {
					seen[e.EntraID] = true
					n++
				}
			}
			wantNative += n
			wantRest += max(re.Count, len(re.Users)) - n
		}
	}
	if item(native, "reactions_native") != wantNative || item(native, "reactions_summarized") != wantRest || wantNative == 0 || wantRest == 0 {
		t.Errorf("native: %d as reactions and %d summarized, want %d and %d", item(native, "reactions_native"), item(native, "reactions_summarized"), wantNative, wantRest)
	}
	var note *preflight.Finding
	for i, n := range native.Notes {
		if n.Code == "content.reactions_native" {
			note = &native.Notes[i]
		}
	}
	if note == nil {
		t.Fatalf("no note about native reactions: %s", codes(native.Notes))
	}
	for _, want := range []string{"has not been run against a real tenant", "summarized in a line of text instead", "audit log"} {
		if !strings.Contains(note.Message, want) {
			t.Errorf("the note does not say %q: %s", want, note.Message)
		}
	}
	if len(native.Warnings) != len(footer.Warnings) || native.Verdict != footer.Verdict {
		t.Errorf("the note changed the verdict: %s, warnings %s", native.Verdict, codes(native.Warnings))
	}

	// Offline nobody is matched, so nothing is counted either way.
	offline := setup(t, "nasty", func(m *config.Config) { m.Content.Reactions = config.ReactionsNative })
	offline.Directory, offline.Offline = nil, true
	if r := run(t, offline); item(r, "reactions_native") != 0 || item(r, "reactions_summarized") != 0 {
		t.Errorf("offline: %d and %d counted", item(r, "reactions_native"), item(r, "reactions_summarized"))
	}
}

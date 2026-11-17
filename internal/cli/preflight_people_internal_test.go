package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// peopleReport is a preflight report whose People section has everything
// in it: every way of matching, matches by name to review, and people left
// unmapped with the reason a matcher did not decide.
func peopleReport() *preflight.Report {
	return &preflight.Report{
		Config: "acme", Verdict: preflight.VerdictWarnings, Strategy: config.StrategyModern,
		Blockers: []preflight.Finding{}, Notes: []preflight.Finding{},
		Warnings: []preflight.Finding{{Code: "identity.weak_matches",
			Message: "3 people were matched to an Entra account by the name part of an email address (2) or by display name (1), not by a full email address. A wrong match attributes one person's messages, mentions and reactions to another, and adds that other person to the team.",
			Next:    "Review them in the People section."}},
		Identity: preflight.IdentityReport{
			Checked: true, Users: 9, MappedByEmail: 3, MappedByOverride: 1, MappedByEmailLocalPart: 2, MappedByDisplayName: 1, Unmapped: 2,
			Matchers: []string{"email_local_part", "display_name"},
			Policy:   config.PolicyFormerMember, Account: "former@acme.example", UnmappedMessages: 12,
			UnmappedUsers: []preflight.UnmappedUser{
				{SlackID: "U8", Name: "John Smith", Email: "john@old.example", Reason: "no_match", Detail: "email not in Entra; their name fits several accounts", Messages: 9},
				{SlackID: "U9", Name: "Deploy Bot", Reason: "bot", Detail: "bot or app", Messages: 3},
			},
			Matched: []preflight.MatchedUser{
				{SlackID: "U5", Name: "Grace Hopper", Email: "ghopper@old.example", Method: "email_local_part", Detail: "name part of the email address", Weak: true,
					MatchedOn: "ghopper", EntraID: "id-5", EntraUPN: "ghopper@acme.example", Messages: 1204},
				{SlackID: "U6", Name: "Linus Torvalds", Email: "", Method: "display_name", Detail: "display name", Weak: true,
					MatchedOn: "linus torvalds", EntraID: "id-6", EntraUPN: "linus@acme.example", TargetDisabled: true, Messages: 7},
				{SlackID: "U7", Name: "Tony Hoare", Email: "car.hoare@old.example", Method: "email_local_part", Detail: "name part of the email address", Weak: true,
					MatchedOn: "car.hoare", EntraID: "id-7", EntraUPN: "tony@acme.example", Messages: 0},
				{SlackID: "U1", Name: "Ada Lovelace", Email: "ada@acme.example", Method: "email", Detail: "email address", EntraID: "id-1", EntraUPN: "ada@acme.example", Messages: 40},
				{SlackID: "U2", Name: "Barbara Liskov", Email: "barbara@acme.example", Method: "email", Detail: "email address", EntraID: "id-2", EntraUPN: "barbara@acme.example", Messages: 2},
				{SlackID: "U3", Name: "Donald Knuth", Email: "don@acme.example", Method: "email", Detail: "email address", EntraID: "id-3", EntraUPN: "don@acme.example", Messages: 5},
				{SlackID: "U4", Name: "Edsger Dijkstra", Email: "edsger@home.example", Method: "override", Detail: "override CSV", EntraID: "id-4", EntraUPN: "edsger@acme.example", Messages: 11},
			},
		},
	}
}

// The People section says how many people each matcher matched, and lists
// the ones matched by a guess with the account each was given.
func TestPreflightPeopleListsMatchesByName(t *testing.T) {
	th := theme.New(theme.Options{})
	r := displayReport(peopleReport())
	out := renderPeople(th, r, 100)
	testutil.Golden(t, "preflight_people_weak_plain", testutil.Plain(out))

	plain := strings.Join(strings.Fields(testutil.Plain(out)), " ")
	for _, want := range []string{
		"Matched by override 1", "Matched by email 3", "Matched by email name part 2", "Matched by display name 1",
		"Matched by name: review these", "ghopper@acme.example", "linus@acme.example (disabled)", "tony@acme.example",
		"entra value is none", "their name fits several accounts",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("the People section does not say %q:\n%s", want, out)
		}
	}
	// People matched by their full address are counted, not listed.
	for _, not := range []string{"Ada Lovelace", "barbara@acme.example", "Edsger Dijkstra"} {
		if strings.Contains(plain, not) {
			t.Errorf("the People section lists %q, who was matched exactly", not)
		}
	}

	// The section is marked for attention, and the document holds it.
	doc := preflightDocument(peopleReport())
	found := false
	for _, s := range doc.Sections {
		if s.Title == "People" {
			found = true
			if s.Status != theme.StatusWarn || s.Count != "2" {
				t.Errorf("People section: status %v, count %q", s.Status, s.Count)
			}
		}
	}
	if !found {
		t.Error("no People section")
	}

	// With the matchers off their rows are not shown at all.
	off := peopleReport()
	off.Identity.Matchers, off.Identity.MappedByEmailLocalPart, off.Identity.MappedByDisplayName = []string{}, 0, 0
	off.Identity.Matched = off.Identity.Matched[3:]
	if out := testutil.Plain(renderPeople(th, displayReport(off), 100)); strings.Contains(out, "name part") || strings.Contains(out, "display name") || strings.Contains(out, "review these") {
		t.Errorf("matchers off, yet:\n%s", out)
	}
}

// The JSON document lists every matched person with the method.
func TestPreflightJSONListsHowEachPersonWasMatched(t *testing.T) {
	raw, err := json.Marshal(displayReport(peopleReport()))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Identity struct {
			Matchers               []string `json:"matchers"`
			MappedByEmailLocalPart int      `json:"mappedByEmailLocalPart"`
			MappedByDisplayName    int      `json:"mappedByDisplayName"`
			Matched                []struct {
				SlackID   string `json:"slackId"`
				Method    string `json:"method"`
				Detail    string `json:"detail"`
				Weak      bool   `json:"weak"`
				MatchedOn string `json:"matchedOn"`
				EntraUPN  string `json:"entraUpn"`
				Disabled  bool   `json:"targetDisabled"`
				Messages  int    `json:"messages"`
			} `json:"matched"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	id := doc.Identity
	if len(id.Matchers) != 2 || id.MappedByEmailLocalPart != 2 || id.MappedByDisplayName != 1 || len(id.Matched) != 7 {
		t.Fatalf("identity: %+v", id)
	}
	methods := map[string]int{}
	for _, m := range id.Matched {
		methods[m.Method]++
		if m.Weak != (m.Method == "email_local_part" || m.Method == "display_name") || m.Weak != (m.MatchedOn != "") || m.EntraUPN == "" || m.Detail == "" {
			t.Errorf("%s: %+v", m.SlackID, m)
		}
	}
	if methods["email"] != 3 || methods["override"] != 1 || methods["email_local_part"] != 2 || methods["display_name"] != 1 {
		t.Errorf("methods: %v", methods)
	}
	if m := id.Matched[1]; m.SlackID != "U6" || !m.Disabled || m.Messages != 7 {
		t.Errorf("second entry: %+v", m)
	}
}

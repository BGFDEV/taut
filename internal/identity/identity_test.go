package identity

import (
	"errors"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/slack"
)

func dir() *Directory {
	return NewDirectory([]DirUser{
		{ID: "id-ada", DisplayName: "Ada", Mail: "ada@acme.com", UPN: "ada@acme.com", Enabled: true},
		{ID: "id-grace", DisplayName: "Grace", Mail: "grace.hopper@acme.com", UPN: "ghopper@acme.com", Enabled: true,
			Aliases: []string{"SMTP:grace.hopper@acme.com", "smtp:grace@old-acme.com"}},
		{ID: "id-old", DisplayName: "Edsger", Mail: "edsger@acme.com", UPN: "edsger@acme.com", Enabled: false},
	})
}

func user(id, email string, mod func(*slack.User)) slack.User {
	u := slack.User{ID: id, Name: strings.ToLower(id)}
	u.Profile.Email = email
	if mod != nil {
		mod(&u)
	}
	return u
}

func TestResolve(t *testing.T) {
	users := []slack.User{
		user("U1", "Ada@Acme.com", nil), // case differs
		user("U2", "grace@old-acme.com", nil),
		user("U3", "edsger@acme.com", nil),
		user("U4", "nobody@acme.com", nil),
		user("U5", "", func(u *slack.User) { u.IsBot = true }),
		user("U6", "gone@acme.com", func(u *slack.User) { u.Deleted = true }),
		user("U7", "", func(u *slack.User) { u.IsStranger = true }),
		user("U8", "", nil),
		user("U9", "dana@personal.example", nil), // override by ID
		user("U10", "sam@acme.com", nil),         // override by email → missing target
		user("U11", "ada@acme.com", func(u *slack.User) { u.Deleted = true }),
	}
	ov := []Override{
		{SlackID: "U9", Entra: "GHOPPER@acme.com"},
		{SlackEmail: "SAM@acme.com", Entra: "sam@nowhere.example"},
	}
	m := Resolve(users, dir(), ov, Options{})

	want := map[string]struct {
		method Method
		entra  string
		reason Reason
	}{
		"U1":  {MethodEmail, "id-ada", ""},
		"U2":  {MethodEmail, "id-grace", ""}, // via proxy address
		"U3":  {MethodEmail, "id-old", ""},
		"U4":  {MethodNone, "", ReasonNoMatch},
		"U5":  {MethodNone, "", ReasonBot},
		"U6":  {MethodNone, "", ReasonDeactivated},
		"U7":  {MethodNone, "", ReasonExternal},
		"U8":  {MethodNone, "", ReasonNoEmail},
		"U9":  {MethodOverride, "id-grace", ""},
		"U10": {MethodNone, "", ReasonOverrideStale},
		"U11": {MethodEmail, "id-ada", ""}, // deactivated in Slack but still employed
	}
	for id, w := range want {
		e := m.Lookup(id)
		if e == nil {
			t.Fatalf("%s: no entry", id)
		}
		if e.Method != w.method || e.EntraID != w.entra || e.Reason != w.reason {
			t.Errorf("%s: got method=%s entra=%q reason=%q, want %s %q %q", id, e.Method, e.EntraID, e.Reason, w.method, w.entra, w.reason)
		}
	}
	if !m.Lookup("U3").TargetDisabled {
		t.Error("U3 maps to a disabled account and should be flagged")
	}
	if c := m.Counts(); c != (Counts{Email: 4, Override: 1, Unmapped: 6}) || c.Total() != len(users) {
		t.Errorf("counts = %+v", c)
	}
	if len(m.Weak()) != 0 {
		t.Errorf("weak matches with the matchers off: %v", m.Weak())
	}
}

func TestNilDirectoryMapsNobody(t *testing.T) {
	m := Resolve([]slack.User{user("U1", "ada@acme.com", func(u *slack.User) { u.RealName = "Ada" })}, nil, nil,
		Options{EmailLocalPart: true, DisplayName: true})
	if m.Lookup("U1").Mapped() {
		t.Fatal("mapped without a directory")
	}
}

// named returns a Slack user with an email address and names.
func named(id, email, realName, displayName string, mod func(*slack.User)) slack.User {
	return user(id, email, func(u *slack.User) {
		u.Profile.RealName, u.Profile.DisplayName = realName, displayName
		if mod != nil {
			mod(u)
		}
	})
}

// weakDir is a directory in which names and local parts collide in every
// way the weak matchers must refuse.
func weakDir() *Directory {
	return NewDirectory([]DirUser{
		{ID: "id-ada", DisplayName: "Ada Lovelace", Mail: "ada@acme.com", UPN: "ada@acme.com", Enabled: true},
		{ID: "id-grace", DisplayName: "Grace Hopper", Mail: "grace.hopper@acme.com", UPN: "ghopper@acme.com", Enabled: true,
			Aliases: []string{"SMTP:grace.hopper@acme.com", "smtp:amazing.grace@acme.com"}},
		{ID: "id-linus", DisplayName: "Linus  Torvalds", Mail: "linus@acme.com", UPN: "linus@acme.com", Enabled: true},
		{ID: "id-kay", DisplayName: "Alan Kay", Mail: "alan.kay@acme.com", UPN: "akay@acme.com", Enabled: false},
		// Two accounts with one name, and two that share a local part
		// across domains.
		{ID: "id-smith1", DisplayName: "John Smith", Mail: "john.smith@acme.com", UPN: "john.smith@acme.com", Enabled: true},
		{ID: "id-smith2", DisplayName: "John Smith", Mail: "jsmith2@acme.com", UPN: "jsmith2@acme.com", Enabled: true},
		{ID: "id-pat1", DisplayName: "Pat One", Mail: "pat@acme.com", UPN: "pat@acme.com", Enabled: true},
		{ID: "id-pat2", DisplayName: "Pat Two", Mail: "pat@acme.co.uk", UPN: "pat@acme.co.uk", Enabled: true},
		// A guest of the tenant: never matched by a guess.
		{ID: "id-guest", DisplayName: "Margaret Hamilton", Mail: "margaret@partner.example", UPN: "margaret_partner.example#EXT#@acme.onmicrosoft.com", Enabled: true, Guest: true},
		{ID: "id-barbara", DisplayName: "Barbara Liskov", Mail: "barbara@acme.com", UPN: "barbara@acme.com", Enabled: true},
		{ID: "id-don", DisplayName: "Donald Knuth", Mail: "don@acme.com", UPN: "don@acme.com", Enabled: true},
		{ID: "id-edsger", DisplayName: "Edsger Dijkstra", Mail: "edsger@acme.com", UPN: "edsger@acme.com", Enabled: true},
		{ID: "id-tony", DisplayName: "Tony Hoare", Mail: "tony@acme.com", UPN: "tony@acme.com", Enabled: true,
			Aliases: []string{"SMTP:tony@acme.com", "smtp:car.hoare@acme.com", "x500:/o=acme/cn=car.hoare"}},
	})
}

func TestWeakMatchers(t *testing.T) {
	users := []slack.User{
		named("U1", "ada@acme.com", "Ada Lovelace", "ada", nil),             // exact email, as before
		named("U2", "GHopper@gmail.com", "G. Hopper", "", nil),              // local part of an account's UPN
		named("U3", "amazing.grace@old.example", "Amazing Grace", "", nil),  // local part of an alias of the same account
		named("U4", "torvalds@personal.example", "linus torvalds", "", nil), // display name, case and spaces aside
		named("U5", "kay@personal.example", "", "Alan Kay", nil),            // display name in Slack's display name; account disabled
		named("U6", "smith@personal.example", "John Smith", "", nil),        // two accounts have the name
		named("U7", "pat@personal.example", "Pat Three", "", nil),           // two accounts have the local part
		named("U8", "margaret@personal.example", "Margaret Hamilton", "", nil),
		named("U9", "barbara@one.example", "B. One", "", nil), // two Slack users, one local part
		named("U10", "barbara@two.example", "B. Two", "", nil),
		named("U11", "ada@other.example", "Ada Lovelace", "", nil), // the account is taken by an exact match
		named("U12", "bot@personal.example", "Donald Knuth", "", func(u *slack.User) { u.IsBot = true }),
		named("U13", "don@partner.example", "Donald Knuth", "", func(u *slack.User) { u.IsRestricted = true }),
		named("U14", "don@elsewhere.example", "Donald Knuth", "", func(u *slack.User) { u.IsStranger = true }),
		named("U15", "don@personal.example", "Someone Else", "", func(u *slack.User) { u.Deleted = true }), // a former member can be matched
		named("U16", "edsger@personal.example", "Edsger Dijkstra", "", nil),                                // rejected in the override CSV
		named("U17", "", "", "", nil),
		named("U18", "nobody@personal.example", "No Body", "", nil),
		named("U19", "linus@personal.example", "Grace Hopper", "", nil), // local part says one account, name another: the stronger wins
		named("U20", "Car.Hoare@personal.example", "C. Hoare", "", nil), // local part of an alias
	}
	ov := []Override{{SlackID: "U16", None: true}}

	type want struct {
		method    Method
		entra     string
		matchedOn string
		undecided string
	}
	check := func(t *testing.T, opts Options, wants map[string]want) *Map {
		t.Helper()
		m := Resolve(users, weakDir(), ov, opts)
		for _, u := range users {
			w, ok := wants[u.ID]
			if !ok {
				w = want{method: MethodNone}
			}
			e := m.Lookup(u.ID)
			if e.Method != w.method || e.EntraID != w.entra || e.MatchedOn != w.matchedOn || e.Undecided != w.undecided {
				t.Errorf("%s: method=%s entra=%q on=%q undecided=%q, want %s %q %q %q", u.ID, e.Method, e.EntraID, e.MatchedOn, e.Undecided,
					w.method, w.entra, w.matchedOn, w.undecided)
			}
			if e.Mapped() != (e.Reason == "") {
				t.Errorf("%s: mapped=%t with reason %q", u.ID, e.Mapped(), e.Reason)
			}
		}
		return m
	}

	t.Run("off", func(t *testing.T) {
		m := check(t, Options{}, map[string]want{"U1": {MethodEmail, "id-ada", "", ""}})
		if e := m.Lookup("U16"); !e.Excluded || e.Reason != ReasonExcluded {
			t.Errorf("U16: %+v", e)
		}
		if e := m.Lookup("U15"); e.Reason != ReasonDeactivated {
			t.Errorf("U15 reason = %q", e.Reason)
		}
	})
	t.Run("email local part", func(t *testing.T) {
		check(t, Options{EmailLocalPart: true}, map[string]want{
			"U1":  {MethodEmail, "id-ada", "", ""},
			"U2":  {MethodNone, "", "", UndecidedSeveralUsers}, // U2 (by UPN) and U3 (by alias) would both get one account
			"U3":  {MethodNone, "", "", UndecidedSeveralUsers},
			"U7":  {MethodNone, "", "", UndecidedSeveralAccounts},
			"U9":  {MethodNone, "", "", UndecidedSeveralUsers},
			"U10": {MethodNone, "", "", UndecidedSeveralUsers},
			"U15": {MethodEmailLocalPart, "id-don", "don", ""},
			"U19": {MethodEmailLocalPart, "id-linus", "linus", ""},
			"U20": {MethodEmailLocalPart, "id-tony", "car.hoare", ""},
		})
	})
	t.Run("display name", func(t *testing.T) {
		check(t, Options{DisplayName: true}, map[string]want{
			"U1":  {MethodEmail, "id-ada", "", ""},
			"U4":  {MethodDisplayName, "id-linus", "linus torvalds", ""},
			"U5":  {MethodDisplayName, "id-kay", "alan kay", ""},
			"U6":  {MethodNone, "", "", UndecidedSeveralAccounts},
			"U19": {MethodDisplayName, "id-grace", "grace hopper", ""},
		})
	})
	t.Run("both", func(t *testing.T) {
		m := check(t, Options{EmailLocalPart: true, DisplayName: true}, map[string]want{
			"U1":  {MethodEmail, "id-ada", "", ""},
			"U2":  {MethodNone, "", "", UndecidedSeveralUsers},
			"U3":  {MethodNone, "", "", UndecidedSeveralUsers},
			"U4":  {MethodNone, "", "", ""}, // Linus's account went to U19 by local part, the stronger matcher
			"U5":  {MethodDisplayName, "id-kay", "alan kay", ""},
			"U6":  {MethodNone, "", "", UndecidedSeveralAccounts},
			"U7":  {MethodNone, "", "", UndecidedSeveralAccounts},
			"U9":  {MethodNone, "", "", UndecidedSeveralUsers},
			"U10": {MethodNone, "", "", UndecidedSeveralUsers},
			"U15": {MethodEmailLocalPart, "id-don", "don", ""},
			"U19": {MethodEmailLocalPart, "id-linus", "linus", ""},
			"U20": {MethodEmailLocalPart, "id-tony", "car.hoare", ""},
		})
		if c := m.Counts(); c != (Counts{Email: 1, EmailLocalPart: 3, DisplayName: 1, Unmapped: 15}) {
			t.Errorf("counts = %+v", c)
		}
		var weak []string
		for _, e := range m.Weak() {
			weak = append(weak, e.SlackID)
			if !e.Method.Weak() || e.MatchedOn == "" {
				t.Errorf("%s: %+v", e.SlackID, e)
			}
		}
		if got := strings.Join(weak, ","); got != "U5,U20,U19,U15" {
			t.Errorf("weak matches = %s", got)
		}
		if !m.Lookup("U5").TargetDisabled {
			t.Error("U5 maps to a disabled account and should be flagged")
		}
		// No account is given to two Slack users by a guess.
		given := map[string]string{}
		for _, e := range m.Entries() {
			if !e.Mapped() {
				continue
			}
			if other, dup := given[e.EntraID]; dup {
				t.Errorf("%s and %s both map to %s", other, e.SlackID, e.EntraID)
			}
			given[e.EntraID] = e.SlackID
		}
	})
}

// A guess never overrides the operator: not a row that names an account,
// not a row whose account is missing, not a row that says there is none.
func TestWeakMatchersYieldToOverrides(t *testing.T) {
	users := []slack.User{
		named("U1", "linus@personal.example", "Linus Torvalds", "", nil),
		named("U2", "don@personal.example", "Donald Knuth", "", nil),
		named("U3", "barbara@personal.example", "Barbara Liskov", "", nil),
		named("U4", "edsger@acme.com", "Edsger Dijkstra", "", nil),
	}
	ov := []Override{
		{SlackID: "U1", Entra: "ada@acme.com"},
		{SlackID: "U2", Entra: "gone@acme.com"},
		{SlackEmail: "barbara@personal.example", None: true},
		{SlackID: "U4", None: true}, // even an exact email match is not made
	}
	m := Resolve(users, weakDir(), ov, Options{EmailLocalPart: true, DisplayName: true})
	if e := m.Lookup("U1"); e.Method != MethodOverride || e.EntraID != "id-ada" {
		t.Errorf("U1: %+v", e)
	}
	if e := m.Lookup("U2"); e.Mapped() || e.Reason != ReasonOverrideStale {
		t.Errorf("U2: %+v", e)
	}
	for _, id := range []string{"U3", "U4"} {
		if e := m.Lookup(id); e.Mapped() || !e.Excluded || e.Reason != ReasonExcluded {
			t.Errorf("%s: %+v", id, e)
		}
	}
}

func TestMethodDescribe(t *testing.T) {
	for _, m := range []Method{MethodEmail, MethodOverride, MethodEmailLocalPart, MethodDisplayName, MethodNone} {
		if d := m.Describe(); d == "" || d == string(m) {
			t.Errorf("%s has no description", m)
		}
		if m.Weak() != (m == MethodEmailLocalPart || m == MethodDisplayName) {
			t.Errorf("%s: Weak() = %t", m, m.Weak())
		}
	}
	if ReasonExcluded.Describe() == string(ReasonExcluded) {
		t.Error("ReasonExcluded has no description")
	}
}

func TestAddUnknown(t *testing.T) {
	m := Resolve(nil, dir(), nil, Options{})
	e := m.AddUnknown("U0ABSENT", "Pat")
	if e.Reason != ReasonNotInExport || e.Name != "Pat" {
		t.Fatalf("%+v", e)
	}
	if m.AddUnknown("U0ABSENT", "other") != e {
		t.Fatal("AddUnknown must be idempotent")
	}
	if m.AddUnknown("USLACKBOT", "").Reason != ReasonBot {
		t.Fatal("Slackbot should be classed as a bot")
	}
}

func TestParseOverrides(t *testing.T) {
	got, err := ParseOverrides(strings.NewReader("\uFEFFSlack_User_ID, slack_email ,entra,note\nU1,,ada@acme.com,moved\n,,,\n,old@x.com,id-ada,\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SlackID != "U1" || got[1].SlackEmail != "old@x.com" || got[1].Entra != "id-ada" {
		t.Fatalf("%+v", got)
	}

	for name, in := range map[string]string{
		"no entra column": "slack_user_id,target\nU1,a@b.c\n",
		"no slack column": "entra\na@b.c\n",
		"row lacks entra": "slack_user_id,entra\nU1,\n",
		"row lacks slack": "slack_user_id,entra\n,a@b.c\n",
	} {
		_, err := ParseOverrides(strings.NewReader(in))
		var fe *fail.Error
		if !errors.As(err, &fe) || fe.Code != "identity.overrides_invalid" || fe.Next == "" {
			t.Errorf("%s: got %v", name, err)
		}
	}

	// "none" says a user has no account. It is not an address to look up.
	got, err = ParseOverrides(strings.NewReader("slack_user_id,slack_email,entra\nU1,,none\n,old@x.com, NONE \nU3,,nobody@acme.com\n"))
	if err != nil || len(got) != 3 || !got[0].None || got[0].Entra != "" || !got[1].None || got[2].None || got[2].Entra != "nobody@acme.com" {
		t.Fatalf("%+v, %v", got, err)
	}

	if got, err := ParseOverrides(strings.NewReader("")); err != nil || got != nil {
		t.Errorf("empty file: %v %v", got, err)
	}
}

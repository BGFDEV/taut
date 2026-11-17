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
	m := Resolve(users, dir(), ov)

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
	byEmail, byOverride, unmapped := m.Counts()
	if byEmail != 4 || byOverride != 1 || unmapped != 6 {
		t.Errorf("counts = %d %d %d", byEmail, byOverride, unmapped)
	}
}

func TestNilDirectoryMapsNobody(t *testing.T) {
	m := Resolve([]slack.User{user("U1", "ada@acme.com", nil)}, nil, nil)
	if m.Lookup("U1").Mapped() {
		t.Fatal("mapped without a directory")
	}
}

func TestAddUnknown(t *testing.T) {
	m := Resolve(nil, dir(), nil)
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

	if got, err := ParseOverrides(strings.NewReader("")); err != nil || got != nil {
		t.Errorf("empty file: %v %v", got, err)
	}
}

package worker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/identity"
)

// TestMigrateWithNameMatchers runs a tenant in which two people of the
// export no longer have the address Slack knows: one kept the name part of
// it, the other only their display name. Without identity.match their
// messages go to the placeholder account. With it they keep their own
// names, and the ledger records how each was matched.
func TestMigrateWithNameMatchers(t *testing.T) {
	var byLocalPart, byName fixtures.Person
	for _, p := range fixtures.People("small") {
		if !p.InDirectory || p.Email == "" {
			continue
		}
		if byLocalPart.SlackID == "" {
			byLocalPart = p
		} else if byName.SlackID == "" {
			byName = p
		}
	}
	if byName.SlackID == "" {
		t.Fatal("the small fixture has fewer than two people in the directory")
	}
	account := map[string]string{} // Slack ID → Entra ID of their account
	placeholder := ""
	move := func(s *fakegraph.Seed) {
		for i := range s.Users {
			u := &s.Users[i]
			switch strings.ToLower(u.Mail) {
			case strings.ToLower(byLocalPart.Email):
				local, _, _ := strings.Cut(u.Mail, "@")
				u.Mail, u.UserPrincipalName = local+"@new-domain.example", local+"@new-domain.example"
				account[byLocalPart.SlackID] = u.ID
			case strings.ToLower(byName.Email):
				u.Mail, u.UserPrincipalName = "employee-0042@new-domain.example", "employee-0042@new-domain.example"
				account[byName.SlackID] = u.ID
			case devstack.FormerMemberUPN:
				placeholder = u.ID
			}
		}
	}

	for _, tc := range []struct {
		name string
		mod  func(*config.Config)
		on   bool
	}{
		{"off", nil, false},
		{"on", func(m *config.Config) { m.Identity.Match = config.Match{EmailLocalPart: true, DisplayName: true} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHarnessSeeded(t, "small", tc.mod, move)
			if len(account) != 2 || placeholder == "" {
				t.Fatalf("the seed was not changed as intended: %v, placeholder %q", account, placeholder)
			}
			ctx := t.Context()
			kill := r.worker(ctx, 5*time.Second)
			defer kill()
			run := r.start(ctx)
			v := r.await(ctx, run.ID, time.Minute)
			w := r.expected()
			r.verify(v, w)

			mapped, err := r.store.Users(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			methods := map[string]string{}
			for _, u := range mapped {
				methods[u.SlackID] = u.Method
				if own, moved := account[u.SlackID]; moved && tc.on && u.EntraID != own {
					t.Errorf("%s is mapped to %s, want their own account %s", u.SlackID, u.EntraID, own)
				}
			}
			wantLocal, wantName := string(identity.MethodNone), string(identity.MethodNone)
			if tc.on {
				wantLocal, wantName = string(identity.MethodEmailLocalPart), string(identity.MethodDisplayName)
			}
			if methods[byLocalPart.SlackID] != wantLocal || methods[byName.SlackID] != wantName {
				t.Errorf("methods: %s by %q and %s by %q, want %q and %q", byLocalPart.SlackID, methods[byLocalPart.SlackID],
					byName.SlackID, methods[byName.SlackID], wantLocal, wantName)
			}

			// Their messages are theirs, or the placeholder's with their
			// name written in the message.
			team := r.fake.Teams()[0]
			checked := 0
			for _, c := range r.fake.Channels(team.ID) {
				name := c.DisplayName
				if name == "General" {
					name = "general"
				}
				msgs, stored := w.byChannel[name], r.fake.Messages(team.ID, c.ID)
				if len(msgs) != len(stored) {
					continue // verify has reported it
				}
				for i, m := range stored {
					own, moved := account[msgs[i].User]
					if !moved {
						continue
					}
					checked++
					switch {
					case tc.on && (m.FromUserID != own || strings.Contains(m.BodyContent, "(via Slack)")):
						t.Errorf("message %s of %s is from %s, want their account %s", msgs[i].TS, msgs[i].User, m.FromUserID, own)
					case !tc.on && (m.FromUserID != placeholder || !strings.Contains(m.BodyContent, "(via Slack)")):
						t.Errorf("message %s of %s is from %s, want the placeholder with a byline", msgs[i].TS, msgs[i].User, m.FromUserID)
					}
				}
			}
			if checked == 0 {
				t.Error("neither person wrote a message in the fixture's channels")
			}
		})
	}
}

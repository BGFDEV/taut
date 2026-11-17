package config

import (
	"reflect"
	"testing"
)

func TestTeamSpecSelects(t *testing.T) {
	team := func(include, exclude, types []string) TeamSpec {
		return TeamSpec{Channels: ChannelSelection{Include: include, Exclude: exclude, Types: types}}
	}
	pub, priv := []string{ChannelPublic}, []string{ChannelPrivate}
	both := []string{ChannelPublic, ChannelPrivate}

	tests := []struct {
		name    string
		team    TeamSpec
		channel string
		kind    string
		want    bool
	}{
		{"star matches anything", team([]string{"*"}, nil, pub), "general", ChannelPublic, true},
		{"exclude wins over include", team([]string{"*"}, []string{"random"}, pub), "random", ChannelPublic, false},
		{"exclude leaves others", team([]string{"*"}, []string{"random"}, pub), "random-2", ChannelPublic, true},
		{"kind not listed", team([]string{"*"}, nil, pub), "leadership", ChannelPrivate, false},
		{"private listed", team([]string{"*"}, nil, priv), "leadership", ChannelPrivate, true},
		{"private only rejects public", team([]string{"*"}, nil, priv), "general", ChannelPublic, false},
		{"both kinds", team([]string{"*"}, nil, both), "leadership", ChannelPrivate, true},
		{"prefix glob hit", team([]string{"eng-*"}, nil, pub), "eng-backend", ChannelPublic, true},
		{"prefix glob miss", team([]string{"eng-*"}, nil, pub), "design", ChannelPublic, false},
		{"glob is anchored", team([]string{"eng"}, nil, pub), "eng-backend", ChannelPublic, false},
		{"exact name", team([]string{"general", "announcements"}, nil, pub), "announcements", ChannelPublic, true},
		{"question mark", team([]string{"team-?"}, nil, pub), "team-a", ChannelPublic, true},
		{"question mark needs one char", team([]string{"team-?"}, nil, pub), "team-ab", ChannelPublic, false},
		{"character class", team([]string{"proj-[0-9]*"}, nil, pub), "proj-42", ChannelPublic, true},
		{"character class miss", team([]string{"proj-[0-9]*"}, nil, pub), "proj-x", ChannelPublic, false},
		{"exclude glob", team([]string{"eng-*"}, []string{"eng-*-archive"}, pub), "eng-web-archive", ChannelPublic, false},
		{"second include", team([]string{"eng-*", "ops"}, nil, pub), "ops", ChannelPublic, true},
		{"empty include means all", team(nil, nil, pub), "anything", ChannelPublic, true},
		{"empty types means public", team([]string{"*"}, nil, nil), "general", ChannelPublic, true},
		{"empty types rejects private", team([]string{"*"}, nil, nil), "leadership", ChannelPrivate, false},
		{"unknown kind", team([]string{"*"}, nil, both), "general", "shared", false},
		{"malformed include never matches", team([]string{"[bad"}, nil, pub), "[bad", ChannelPublic, false},
		{"malformed exclude never excludes", team([]string{"*"}, []string{"[bad"}, pub), "general", ChannelPublic, true},
		{"case sensitive", team([]string{"General"}, nil, pub), "general", ChannelPublic, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.team.Selects(tt.channel, tt.kind); got != tt.want {
				t.Errorf("Selects(%q, %q) = %v, want %v", tt.channel, tt.kind, got, tt.want)
			}
		})
	}
}

func TestTeamSpecTargetName(t *testing.T) {
	team := TeamSpec{Channels: ChannelSelection{Rename: map[string]string{"old-name": "New Name", "blank": ""}}}
	tests := []struct{ in, want string }{
		{"old-name", "New Name"},
		{"general", "general"},
		{"blank", "blank"},
		{"New Name", "New Name"},
	}
	for _, tt := range tests {
		if got := team.TargetName(tt.in); got != tt.want {
			t.Errorf("TargetName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := (TeamSpec{}).TargetName("x"); got != "x" {
		t.Errorf("TargetName with no rename map = %q", got)
	}
}

func TestSelectingTeams(t *testing.T) {
	m := Default()
	all, eng := DefaultTeam(), DefaultTeam()
	all.Channels.Exclude = []string{"eng-secret"}
	eng.Channels.Include = []string{"eng-*"}
	m.Target.Teams = []TeamSpec{all, eng}

	tests := []struct {
		channel, kind string
		want          []int
	}{
		{"general", ChannelPublic, []int{0}},
		{"eng-web", ChannelPublic, []int{0, 1}},
		{"eng-secret", ChannelPublic, []int{1}},
		{"eng-web", ChannelPrivate, nil},
	}
	for _, tt := range tests {
		if got := m.SelectingTeams(tt.channel, tt.kind); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SelectingTeams(%q, %q) = %v, want %v", tt.channel, tt.kind, got, tt.want)
		}
	}
}

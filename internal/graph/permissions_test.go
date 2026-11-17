package graph_test

import (
	"fmt"
	"testing"

	"github.com/bgfdev/taut/internal/graph"
)

func TestPermissionCatalog(t *testing.T) {
	cat := graph.RequiredPermissions()
	seen := map[string]bool{}
	for _, p := range cat {
		if p.Name == "" || p.Purpose == "" || p.Requirement == "" || seen[p.Name] {
			t.Errorf("bad or duplicate catalog entry: %+v", p)
		}
		seen[p.Name] = true
	}
	if cat[0].Name != "Teamwork.Migrate.All" || cat[0].RoleID != "dfb0dd15-61de-45b2-be36-d6a69fba3c79" {
		t.Errorf("first entry = %+v", cat[0])
	}
	for _, name := range []string{"User.Read.All", "Team.Create", "Channel.Create", "TeamMember.ReadWrite.All",
		"ChannelMessage.Read.All", "Files.ReadWrite.All", "Sites.ReadWrite.All", "ChannelMember.ReadWrite.All",
		"Chat.Create", "Chat.ReadBasic.All", "Chat.Read.All"} {
		if !seen[name] {
			t.Errorf("catalog is missing %s", name)
		}
	}
	// The catalog is a copy: callers cannot corrupt it.
	cat[0].Name = "mutated"
	if graph.RequiredPermissions()[0].Name != "Teamwork.Migrate.All" {
		t.Error("RequiredPermissions returned shared state")
	}
}

func TestCompare(t *testing.T) {
	granted := []string{"teamwork.migrate.all", "User.Read.All", "Chat.Create", "Sites.FullControl.All", "Team.ReadBasic.All"}
	state := func(features graph.FeatureSet) map[string]string {
		out := map[string]string{}
		for _, s := range graph.Compare(granted, features) {
			out[s.Name] = fmt.Sprintf("%s/%v", s.State, s.Granted)
		}
		return out
	}

	base := state(graph.FeatureSet{})
	for name, want := range map[string]string{
		"Teamwork.Migrate.All":        "granted/true", // case-insensitive
		"User.Read.All":               "granted/true",
		"Team.Create":                 "missing-required/false",
		"Files.ReadWrite.All":         "missing-required/false",
		"ChannelMember.ReadWrite.All": "not-needed/false",
		"Chat.Create":                 "not-needed/true",
		"Chat.ReadBasic.All":          "not-needed/false",
		"Organization.Read.All":       "missing-optional/false",
		"Team.ReadBasic.All":          "granted/true",
	} {
		if base[name] != want {
			t.Errorf("no features: %s = %s, want %s", name, base[name], want)
		}
	}

	all := state(graph.FeatureSet{PrivateChannels: true, DirectMessages: true})
	for name, want := range map[string]string{
		"ChannelMember.ReadWrite.All": "missing-required/false",
		"Chat.Create":                 "granted/true",
		"Chat.ReadBasic.All":          "missing-required/false",
		"Organization.Read.All":       "missing-optional/false",
	} {
		if all[name] != want {
			t.Errorf("all features: %s = %s, want %s", name, all[name], want)
		}
	}

	// Stable order, one row per catalog entry, whatever the input order.
	a := graph.Compare(granted, graph.FeatureSet{})
	b := graph.Compare([]string{"Team.ReadBasic.All", "Chat.Create", "User.Read.All", "Teamwork.Migrate.All"}, graph.FeatureSet{})
	if len(a) != len(graph.RequiredPermissions()) || len(a) != len(b) {
		t.Fatalf("row counts: %d, %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Name != graph.RequiredPermissions()[i].Name || a[i] != b[i] {
			t.Errorf("row %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}

	if got := graph.MissingRequired(a); fmt.Sprint(got) != "[Team.Create Channel.Create TeamMember.ReadWrite.All ChannelMessage.Read.All Files.ReadWrite.All Sites.ReadWrite.All]" {
		t.Errorf("MissingRequired = %v", got)
	}
	if got := graph.Unrecognized(granted); fmt.Sprint(got) != "[Sites.FullControl.All]" {
		t.Errorf("Unrecognized = %v", got)
	}
	if got := graph.Compare(nil, graph.FeatureSet{}); len(graph.MissingRequired(got)) != 8 {
		t.Errorf("nothing granted: %v", graph.MissingRequired(got))
	}
}

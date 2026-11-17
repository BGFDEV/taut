// Package devstack wires the fixtures and the fake servers together for
// local development, demos and end-to-end tests.
package devstack

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fakeslack"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/slackapi"
)

// Well-known values of the fake tenant, usable in config files that target it.
const (
	Domain          = "fixture.example"
	OwnerUPN        = "admin@" + Domain
	FormerMemberUPN = "former-member@" + Domain
	// SlackToken is accepted by the fake Slack API. It is not a real token.
	SlackToken = "xoxb-FAKE-0000000000-taut-local-dev" //nolint:gosec // accepted only by the fake server
)

// Seed returns a fake tenant whose directory matches the named fixture:
// every fixture person marked InDirectory exists with the same email, plus
// a team owner and a "former member" placeholder account.
func Seed(fixture string) fakegraph.Seed {
	s := fakegraph.DefaultSeed()
	s.Organization = fakegraph.Organization{
		DisplayName: "Fixture Corp",
		Domains:     []fakegraph.Domain{{Name: Domain, IsDefault: true}},
	}
	s.Teams = nil
	s.Users = []fakegraph.User{
		{ID: guid(900001), DisplayName: "Taut Admin", Mail: OwnerUPN, UserPrincipalName: OwnerUPN, AccountEnabled: true, UserType: "Member"},
		{ID: guid(900002), DisplayName: "Former member", Mail: FormerMemberUPN, UserPrincipalName: FormerMemberUPN, AccountEnabled: true, UserType: "Member"},
	}
	n := 0
	for _, p := range fixtures.People(fixture) {
		if !p.InDirectory || p.Email == "" {
			continue
		}
		n++
		// Entra stores addresses in lower case; Slack does not always.
		email := strings.ToLower(p.Email)
		s.Users = append(s.Users, fakegraph.User{
			ID: guid(n), DisplayName: p.DisplayName, Mail: email, UserPrincipalName: email,
			AccountEnabled: true, UserType: "Member",
		})
	}
	return s
}

func guid(n int) string { return fmt.Sprintf("f1c7e000-0000-4000-8000-%012d", n) }

// Handler serves the fake Entra and Graph endpoints at the root and the
// fake Slack Web API under /slack/api, so one local port stands in for both
// clouds.
func Handler(fixture string, opts ...fakegraph.Option) http.Handler {
	mux := http.NewServeMux()
	slack := fakeslack.New("fixture-corp", map[string]fakeslack.Token{
		SlackToken: {Scopes: slackapi.Required(slackapi.Features{PrivateChannels: true})},
	})
	mux.Handle("/slack/api/", http.StripPrefix("/slack/api", slack))
	// Slack's file host, serving generated content for the fixture's files.
	// Sizes are capped so a fixture that describes gigabytes stays quick.
	// A load fixture's files are known by their IDs, so the export, which
	// may be very large, is not generated here to find them.
	if fixtures.IsLoad(fixture) {
		mux.Handle("/files-pri/", fakeslack.NewComputedFileHost(fixtures.LoadFileSize))
	} else if sizes, err := fixtureFiles(fixture); err == nil {
		mux.Handle("/files-pri/", fakeslack.NewFileHost(sizes))
	}
	mux.Handle("/", fakegraph.New(Seed(fixture), opts...))
	return mux
}

// fileCap is the most the fake file host serves for any one file.
const fileCap = 256 << 10

// fixtureFiles lists the downloadable files of a fixture export.
func fixtureFiles(fixture string) (map[string]int64, error) {
	dir, err := os.MkdirTemp("", "taut-fixture-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path, err := fixtures.Write(fixture, dir)
	if err != nil {
		return nil, err
	}
	return fakeslack.FilesFromExport(path, fileCap)
}

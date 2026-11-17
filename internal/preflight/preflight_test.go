package preflight_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/identity"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
)

const (
	owner  = "admin@fixture.example"
	former = "former-member@fixture.example"
)

func directoryFor(fixture string) *identity.Directory {
	users := []identity.DirUser{
		{ID: "owner", UPN: owner, Mail: owner, Enabled: true},
		{ID: "former", UPN: former, Mail: former, Enabled: true},
	}
	for _, p := range fixtures.People(fixture) {
		if p.InDirectory && p.Email != "" {
			e := strings.ToLower(p.Email)
			users = append(users, identity.DirUser{ID: "id-" + p.SlackID, DisplayName: p.DisplayName, Mail: e, UPN: e, Enabled: true})
		}
	}
	return identity.NewDirectory(users)
}

func setup(t *testing.T, fixture string, mod func(*config.Config)) preflight.Options {
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

	m := config.Default()
	m.Metadata.Name = "test"
	m.Source.Slack.Export = path
	m.Source.Slack.API.Enabled = true
	team := config.DefaultTeam()
	team.Name, team.Owner = "Fixture", owner
	m.Target.Teams = []config.TeamSpec{team}
	m.Identity.Unmapped.Policy = config.PolicyFormerMember
	m.Identity.Unmapped.Account = former
	if mod != nil {
		mod(m)
	}
	return preflight.Options{
		Config: m, Export: ex, Directory: directoryFor(fixture),
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
}

func run(t *testing.T, o preflight.Options) *preflight.Report {
	t.Helper()
	r, err := preflight.Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func item(r *preflight.Report, key string) int {
	for _, c := range r.Content {
		if c.Key == key {
			return c.Count
		}
	}
	return 0
}

func codes(fs []preflight.Finding) string {
	var c []string
	for _, f := range fs {
		c = append(c, f.Code)
	}
	return strings.Join(c, ",")
}

// channelMessages is how many of a fixture's messages are in public
// channels.
func channelMessages(f fixtures.FixtureFacts) int {
	n := 0
	for _, c := range f.PerChannel {
		if c.Kind == "public" {
			n += c.Messages
		}
	}
	return n
}

func withDMs(m *config.Config) { m.Target.DirectMessages.Enabled = true }

func TestSmallIsReady(t *testing.T) {
	r := run(t, setup(t, "small", nil))
	f := fixtures.Facts("small")
	if r.Verdict != preflight.VerdictReady {
		t.Fatalf("verdict = %s; blockers=%s warnings=%s", r.Verdict, codes(r.Blockers), codes(r.Warnings))
	}
	// The fixture's direct message and group DM are not migrated unless
	// asked for, so the totals are those of its channels.
	messages := channelMessages(f)
	if r.Totals.Channels != f.PublicChannels || r.Totals.Messages != messages || r.Totals.Replies != f.Replies {
		t.Errorf("totals = %+v, facts = %d channels %d messages %d replies", r.Totals, f.PublicChannels, messages, f.Replies)
	}
	if r.Totals.Chats != 0 || r.Totals.ChatMessages != 0 || r.DirectMessages == nil || r.DirectMessages.Enabled || r.DirectMessages.InExport != 2 {
		t.Errorf("direct messages are off, yet: totals %+v, report %+v", r.Totals, r.DirectMessages)
	}
	if r.Totals.Files != f.Files || r.Totals.FileBytes != f.FileBytes {
		t.Errorf("files = %d (%d bytes), want %d (%d)", r.Totals.Files, r.Totals.FileBytes, f.Files, f.FileBytes)
	}
	if r.Identity.Unmapped != 0 || r.Identity.MappedByEmail != f.Users {
		t.Errorf("identity = %+v", r.Identity)
	}
	if r.Estimate.Imports != messages || r.Estimate.ConservativeSeconds < r.Estimate.OptimisticSeconds {
		t.Errorf("estimate = %+v", r.Estimate)
	}
}

func TestMediumSelectionAndIdentity(t *testing.T) {
	r := run(t, setup(t, "medium", nil))
	f := fixtures.Facts("medium")
	if r.Totals.Channels != f.PublicChannels {
		t.Errorf("channels = %d, want the %d public ones", r.Totals.Channels, f.PublicChannels)
	}
	// Private channels and DMs are in the export but not selected.
	if len(r.Skipped) != f.PrivateChannels+1 {
		t.Errorf("skipped = %+v", r.Skipped)
	}
	if r.Identity.MappedByEmail != f.DirectoryUsers {
		t.Errorf("mapped by email = %d, want %d", r.Identity.MappedByEmail, f.DirectoryUsers)
	}
	if r.Verdict != preflight.VerdictWarnings || !strings.Contains(codes(r.Warnings), "identity.unmapped") {
		t.Errorf("verdict = %s, warnings = %s", r.Verdict, codes(r.Warnings))
	}
	// Each bot must be one author, whether a message names it by user or bot ID.
	seen := map[string]int{}
	for _, u := range r.Identity.UnmappedUsers {
		seen[u.Name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("%q is listed %d times", name, n)
		}
	}

	// Including private channels brings them into the plan.
	withPrivate := run(t, setup(t, "medium", func(m *config.Config) {
		m.Target.Teams[0].Channels.Types = []string{config.ChannelPublic, config.ChannelPrivate}
	}))
	if withPrivate.Totals.Channels != f.PublicChannels+f.PrivateChannels {
		t.Errorf("with private: channels = %d", withPrivate.Totals.Channels)
	}
}

func TestNastyEdgeCases(t *testing.T) {
	r := run(t, setup(t, "nasty", nil))
	f := fixtures.Facts("nasty")

	if item(r, "malformed") != f.MalformedMessages {
		t.Errorf("malformed = %d, want %d", item(r, "malformed"), f.MalformedMessages)
	}
	// The fixture counts collisions in every conversation, and one of them
	// is in a direct message; with direct messages migrated, all are seen.
	if with := run(t, setup(t, "nasty", withDMs)); item(with, "timestamp_collisions") != f.SameMillisecondCollisions {
		t.Errorf("collisions = %d, want %d", item(with, "timestamp_collisions"), f.SameMillisecondCollisions)
	}
	if item(r, "timestamp_collisions") != f.SameMillisecondCollisions-1 {
		t.Errorf("collisions in channels = %d, want %d", item(r, "timestamp_collisions"), f.SameMillisecondCollisions-1)
	}
	for _, key := range []string{"oversized", "edited", "files_external", "canvases", "huddles", "tombstones", "unknown_subtype", "pins", "system", "deleted", "edit_log", "bots"} {
		if item(r, key) == 0 {
			t.Errorf("expected the nasty fixture to produce a %q content item", key)
		}
	}
	// The 150 KB message needs more than one post.
	if r.Estimate.Imports <= r.Totals.Messages {
		t.Errorf("imports (%d) should exceed messages (%d) because of splitting", r.Estimate.Imports, r.Totals.Messages)
	}
	if !strings.Contains(r.Estimate.BoundBy, "megathread") {
		t.Errorf("estimate should be bound by the 2,500-reply thread's channel, got %q", r.Estimate.BoundBy)
	}

	// Teams-illegal names are rewritten, uniquely.
	names := map[string]string{}
	seen := map[string]bool{}
	for _, c := range r.Teams[0].Channels {
		names[c.SlackName] = c.TargetName
		if seen[strings.ToLower(c.TargetName)] {
			t.Errorf("duplicate target name %q", c.TargetName)
		}
		seen[strings.ToLower(c.TargetName)] = true
		if len([]rune(c.TargetName)) > 50 {
			t.Errorf("%q is longer than Teams allows", c.TargetName)
		}
	}
	for slackName, want := range map[string]string{"_archive": "archive", "forms": "forms-channel", "con": "con-channel", "general": "General"} {
		if names[slackName] != want {
			t.Errorf("#%s → %q, want %q", slackName, names[slackName], want)
		}
	}

	// Mixed-case Slack email must still match (Dev.Dubois@Fixture.Example).
	for _, u := range r.Identity.UnmappedUsers {
		if strings.EqualFold(u.Email, "dev.dubois@fixture.example") {
			t.Error("email matching must be case-insensitive")
		}
	}
	// Authors absent from users.json are reported, not dropped.
	if !strings.Contains(codes(r.Warnings), "identity.unmapped") {
		t.Errorf("warnings = %s", codes(r.Warnings))
	}
}

func TestBlockers(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*config.Config)
		opt  func(*preflight.Options)
		code string
	}{
		{"policy block with unmapped authors", func(m *config.Config) { m.Identity.Unmapped.Policy = config.PolicyBlock }, nil, "identity.unmapped"},
		{"unmapped account missing", func(m *config.Config) { m.Identity.Unmapped.Account = "ghost@fixture.example" }, nil, "identity.account_missing"},
		{"owner missing", func(m *config.Config) { m.Target.Teams[0].Owner = "ghost@fixture.example" }, nil, "plan.owner_missing"},
		{"nothing selected", func(m *config.Config) { m.Target.Teams[0].Channels.Include = []string{"no-such-*"} }, nil, "plan.empty"},
		{"directory unavailable", nil, func(o *preflight.Options) { o.Directory = nil }, "identity.unchecked"},
		{"channel in two teams", func(m *config.Config) {
			second := m.Target.Teams[0]
			second.Name = "Second"
			m.Target.Teams = append(m.Target.Teams, second)
		}, nil, "plan.channel_ambiguous"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := setup(t, "medium", c.mod)
			if c.opt != nil {
				c.opt(&o)
			}
			r := run(t, o)
			if r.Verdict != preflight.VerdictBlocked || !strings.Contains(codes(r.Blockers), c.code) {
				t.Fatalf("verdict=%s blockers=%s, want blocked with %s", r.Verdict, codes(r.Blockers), c.code)
			}
			for _, b := range r.Blockers {
				if b.Next == "" {
					t.Errorf("blocker %s has no next step", b.Code)
				}
			}
		})
	}
}

// Private channels and the legacy strategy change who can see what, and
// when. The report says so, without turning either into a warning: there is
// nothing to fix.
func TestNotes(t *testing.T) {
	r := run(t, setup(t, "medium", nil))
	if len(r.Notes) != 0 || r.Strategy != config.StrategyModern {
		t.Errorf("plain config: strategy %q, notes %s", r.Strategy, codes(r.Notes))
	}
	warnings := len(r.Warnings)

	r = run(t, setup(t, "medium", func(m *config.Config) {
		m.Target.Teams[0].Channels.Types = []string{config.ChannelPublic, config.ChannelPrivate}
	}))
	if codes(r.Notes) != "plan.private_members_join_team" {
		t.Fatalf("with private channels: notes %s", codes(r.Notes))
	}
	if n := r.Notes[0]; !strings.Contains(n.Message, "3 private channels") || !strings.Contains(n.Message, "added to the team as well as to the channel") || n.Next == "" {
		t.Errorf("note = %+v", n)
	}
	if len(r.Warnings) != warnings || r.Verdict == preflight.VerdictBlocked {
		t.Errorf("the note changed the verdict: %s, warnings %s", r.Verdict, codes(r.Warnings))
	}

	legacy := func(m *config.Config) { m.Load.Strategy = config.StrategyLegacy }
	r = run(t, setup(t, "medium", legacy))
	if codes(r.Notes) != "load.legacy" || r.Strategy != config.StrategyLegacy {
		t.Fatalf("legacy: strategy %q, notes %s", r.Strategy, codes(r.Notes))
	}
	for _, want := range []string{"until `taut finalize`", "Private channels cannot be migrated", "already exists cannot be imported into", "for good"} {
		if !strings.Contains(r.Notes[0].Message, want) {
			t.Errorf("the legacy note does not say %q: %s", want, r.Notes[0].Message)
		}
	}
	if len(r.Warnings) != warnings {
		t.Errorf("legacy added warnings: %s", codes(r.Warnings))
	}
}

// An existing team of the same name is something the modern strategy
// imports into, with a warning. The legacy strategy cannot.
func TestExistingTeam(t *testing.T) {
	o := setup(t, "small", nil)
	o.ExistingTeams = []string{"fixture"}
	r := run(t, o)
	if r.Verdict == preflight.VerdictBlocked || !strings.Contains(codes(r.Warnings), "team.exists") {
		t.Errorf("modern: verdict %s, warnings %s", r.Verdict, codes(r.Warnings))
	}

	o = setup(t, "small", func(m *config.Config) { m.Load.Strategy = config.StrategyLegacy })
	o.ExistingTeams = []string{"fixture"}
	r = run(t, o)
	if r.Verdict != preflight.VerdictBlocked || !strings.Contains(codes(r.Blockers), "team.exists_legacy") || strings.Contains(codes(r.Warnings), "team.exists") {
		t.Fatalf("legacy: verdict %s, blockers %s, warnings %s", r.Verdict, codes(r.Blockers), codes(r.Warnings))
	}
	for _, b := range r.Blockers {
		if b.Code == "team.exists_legacy" && (b.Next == "" || !strings.Contains(b.Message, "cannot import into an existing team")) {
			t.Errorf("blocker = %+v", b)
		}
	}
}

func TestOfflineWarnsInsteadOfBlocking(t *testing.T) {
	o := setup(t, "small", nil)
	o.Directory, o.Offline = nil, true
	r := run(t, o)
	if r.Verdict != preflight.VerdictWarnings || !strings.Contains(codes(r.Warnings), "identity.unchecked") {
		t.Fatalf("verdict=%s warnings=%s", r.Verdict, codes(r.Warnings))
	}
}

func TestDateRange(t *testing.T) {
	all := run(t, setup(t, "medium", nil))
	some := run(t, setup(t, "medium", func(m *config.Config) { m.Content.Since, m.Content.Until = "2024-04-01", "2024-04-30" }))
	if some.Totals.Messages == 0 || some.Totals.Messages >= all.Totals.Messages {
		t.Fatalf("date range did not narrow the scan: %d of %d", some.Totals.Messages, all.Totals.Messages)
	}
	if some.Totals.First.Before(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)) || some.Totals.Last.After(time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("range = %v .. %v", some.Totals.First, some.Totals.Last)
	}
}

func TestTeamsChannelName(t *testing.T) {
	for in, want := range map[string]string{
		"general-chat": "general-chat", "_archive": "archive", "forms": "forms-channel", "COM1": "COM1-channel",
		"a#b&c": "a-b-c", "x..y": "x.y", "trailing.": "trailing", "__": "channel",
		strings.Repeat("a", 80): strings.Repeat("a", 50),
	} {
		got, reason := preflight.TeamsChannelName(in)
		if got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
		if (got != in) != (reason != "") {
			t.Errorf("%q: reason %q does not match whether the name changed", in, reason)
		}
	}
}

// Slack accepts no control character in a channel name, a user name or an
// ID, so an export that has one was altered or is damaged. preflight says so:
// a blocker for a channel that would migrate, a warning for people.
func TestControlCharactersInTheExportAreReported(t *testing.T) {
	esc := "\x1b]0;owned\x07"
	index := func(v any) *fstest.MapFile {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return &fstest.MapFile{Data: data}
	}
	ex, err := export.OpenFS(fstest.MapFS{
		"users.json": index([]slack.User{
			{ID: "U1", Name: "alice", Profile: slack.Profile{Email: "alice@fixture.example"}},
			{ID: "U2", Name: "mal" + esc + "lory", Profile: slack.Profile{DisplayName: "Mallory\r"}},
			{ID: "U3", Name: "bob", RealName: "Bob\x1b[2J"},
		}),
		"channels.json": index([]slack.Channel{
			{ID: "C1", Name: "general", IsGeneral: true},
			{ID: "C2", Name: "ran" + esc + "dom"},
		}),
	}, "test export")
	if err != nil {
		t.Fatal(err)
	}
	o := setup(t, "small", nil)
	o.Export = ex
	r := run(t, o)

	if !strings.Contains(codes(r.Blockers), "export.channel_control_chars") {
		t.Errorf("blockers: %s", codes(r.Blockers))
	}
	if !strings.Contains(codes(r.Warnings), "export.user_control_chars") {
		t.Errorf("warnings: %s", codes(r.Warnings))
	}
	for _, f := range append(r.Blockers, r.Warnings...) {
		if !strings.HasPrefix(f.Code, "export.") {
			continue
		}
		if strings.ContainsAny(f.Message+f.Next, "\x1b\x07\r") || f.Next == "" {
			t.Errorf("finding %s is not safe to print or has no next step: %q / %q", f.Code, f.Message, f.Next)
		}
		if f.Code == "export.user_control_chars" && !strings.HasPrefix(f.Message, "2 people") {
			t.Errorf("message = %q", f.Message)
		}
		if f.Code == "export.channel_control_chars" && !strings.Contains(f.Message, "#random (C2)") {
			t.Errorf("message = %q", f.Message)
		}
	}
	if r.Verdict != preflight.VerdictBlocked {
		t.Errorf("verdict = %s", r.Verdict)
	}
}

// Direct messages go into chats only when asked for, and then only the
// conversations whose every participant can be in the chat as themselves.
func TestDirectMessages(t *testing.T) {
	r := run(t, setup(t, "small", withDMs))
	f := fixtures.Facts("small")
	dm := r.DirectMessages
	if dm == nil || !dm.Enabled || !dm.IdentityChecked || dm.OneOnOne != 1 || dm.Group != 1 || dm.Skipped != 0 || dm.InExport != 2 ||
		dm.Messages != f.Messages-channelMessages(f) || len(dm.Conversations) != 2 {
		t.Fatalf("direct messages = %+v", dm)
	}
	// Channels are counted as before; chats beside them.
	if r.Totals.Channels != f.PublicChannels || r.Totals.Messages != channelMessages(f) || r.Totals.Chats != 2 || r.Totals.ChatMessages != dm.Messages {
		t.Errorf("totals = %+v", r.Totals)
	}
	if r.Estimate.Imports != f.Messages {
		t.Errorf("estimate imports = %d, want every message of the export, %d", r.Estimate.Imports, f.Messages)
	}
	// It is a private matter: the report says so, as a warning, and tells
	// the operator what will happen.
	if r.Verdict != preflight.VerdictWarnings || codes(r.Warnings) != "dm.private" {
		t.Fatalf("verdict = %s; warnings = %s", r.Verdict, codes(r.Warnings))
	}
	msg := r.Warnings[0].Message
	for _, want := range []string{"private conversations", "visible to its participants while the import runs", "the chat the two people already have", "Files are not moved", "thread replies become ordinary messages"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the warning does not say %q: %s", want, msg)
		}
	}
	for _, c := range dm.Conversations {
		if !c.Migrates() || c.Label == "" || c.Messages == 0 || c.Topic != "" || c.Participants < 2 {
			t.Errorf("conversation = %+v", c)
		}
	}
	// No message content anywhere in the report.
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Direct message 1 of day 1") || strings.Contains(string(raw), "Group message") {
		t.Error("the report repeats the text of a direct message")
	}
}

func TestDirectMessagesSkippedWithReasons(t *testing.T) {
	r := run(t, setup(t, "nasty", withDMs))
	dm := r.DirectMessages
	if dm == nil || dm.OneOnOne != 2 || dm.Group != 1 || dm.Skipped != 6 || dm.InExport != 9 {
		t.Fatalf("direct messages = %+v", dm)
	}
	got := map[string]int{}
	for _, s := range dm.SkippedByReason {
		got[s.Code] = s.Count
		if s.Reason == "" || s.Reason == s.Code {
			t.Errorf("reason %s has no words", s.Code)
		}
	}
	want := map[string]int{
		preflight.DMSkipSelf: 1, preflight.DMSkipBot: 1, preflight.DMSkipGuest: 1,
		preflight.DMSkipExternal: 1, preflight.DMSkipDeactivated: 1, preflight.DMSkipUnmapped: 1,
	}
	if len(got) != len(want) {
		t.Errorf("skipped by reason = %v, want %v", got, want)
	}
	for code, n := range want {
		if got[code] != n {
			t.Errorf("%d conversations skipped as %s, want %d", got[code], code, n)
		}
	}
	// Migrating conversations come first, and every skipped one says why.
	for i, c := range dm.Conversations {
		if c.Migrates() != (i < 3) {
			t.Errorf("conversation %d (%s): migrates = %v", i, c.SlackID, c.Migrates())
		}
		if !c.Migrates() && (c.Reason == "" || c.Messages != 0) {
			t.Errorf("skipped conversation = %+v", c)
		}
	}
	if dm.Replies < 4 || dm.Files != 2 || item(r, "dm_thread_replies") != dm.Replies || item(r, "dm_files") != 2 {
		t.Errorf("thread replies = %d, files = %d, content items %d and %d", dm.Replies, dm.Files, item(r, "dm_thread_replies"), item(r, "dm_files"))
	}
	if !strings.Contains(codes(r.Notes), "dm.skipped") || !strings.Contains(codes(r.Warnings), "dm.private") {
		t.Errorf("notes = %s, warnings = %s", codes(r.Notes), codes(r.Warnings))
	}
	// A direct message's files are not staged.
	without := run(t, setup(t, "nasty", nil))
	if r.Estimate.StagingBytes != without.Estimate.StagingBytes || r.Totals.Files != without.Totals.Files {
		t.Errorf("direct messages changed what is staged: %d bytes and %d files, without them %d and %d",
			r.Estimate.StagingBytes, r.Totals.Files, without.Estimate.StagingBytes, without.Totals.Files)
	}

	// With direct messages off none of this is looked at: one line says
	// they are not migrated.
	if without.DirectMessages == nil || without.DirectMessages.Enabled || without.Totals.Chats != 0 ||
		strings.Contains(codes(without.Warnings), "dm.") || strings.Contains(codes(without.Notes), "dm.") {
		t.Errorf("direct messages off: %+v, warnings %s", without.DirectMessages, codes(without.Warnings))
	}
	found := false
	for _, s := range without.Skipped {
		found = found || (s.Kind == "dm" && strings.Contains(s.Reason, "turned off"))
	}
	if !found {
		t.Errorf("skipped = %+v", without.Skipped)
	}
	for _, s := range r.Skipped {
		if s.Kind == "dm" {
			t.Errorf("with direct messages on, they are still listed as off: %+v", s)
		}
	}
}

// Offline, nobody can be ruled out for lacking an account; what a person
// is in Slack still counts.
func TestDirectMessagesOffline(t *testing.T) {
	o := setup(t, "nasty", withDMs)
	o.Directory, o.Offline = nil, true
	r := run(t, o)
	dm := r.DirectMessages
	if dm == nil || dm.IdentityChecked || dm.Skipped != 5 || dm.OneOnOne != 3 {
		t.Fatalf("direct messages offline = %+v", dm)
	}
	for _, s := range dm.SkippedByReason {
		if s.Code == preflight.DMSkipUnmapped {
			t.Error("a conversation was ruled out for an account nobody looked for")
		}
	}
}

func TestDirectMessageRules(t *testing.T) {
	user := func(id string, mod func(*slack.User)) slack.User {
		u := slack.User{ID: id, Name: strings.ToLower(id), Profile: slack.Profile{Email: strings.ToLower(id) + "@fixture.example", DisplayName: "Person " + id}}
		if mod != nil {
			mod(&u)
		}
		return u
	}
	users := []slack.User{
		user("UA", nil), user("UB", nil), user("UC", nil),
		user("UBOT", func(u *slack.User) { u.IsBot = true }),
		user("UAPP", func(u *slack.User) { u.IsAppUser = true }),
		user("UGUEST", func(u *slack.User) { u.IsUltraRestricted = true }),
		user("UGONE", func(u *slack.User) { u.Deleted = true }),
		user("USTRANGER", func(u *slack.User) { u.IsStranger = true }),
		user("UNOACCOUNT", nil),
		user("UDISABLED", nil),
		user("UTWIN", func(u *slack.User) { u.Profile.Email = "ua@fixture.example" }),
		user("UINVITED", nil),
		user("UUNCLEAR", nil),
	}
	var dir []identity.DirUser
	for _, id := range []string{"UA", "UB", "UC", "UBOT", "UAPP", "UGUEST", "UGONE", "USTRANGER", "UDISABLED"} {
		mail := strings.ToLower(id) + "@fixture.example"
		dir = append(dir, identity.DirUser{ID: "id-" + id, Mail: mail, UPN: mail, Enabled: id != "UDISABLED"})
	}
	// An Entra guest account: somebody from outside, invited into the
	// tenant. And an address two accounts answer to.
	dir = append(dir,
		identity.DirUser{ID: "id-UINVITED", Mail: "uinvited@fixture.example", UPN: "uinvited_fixture.example#EXT#@tenant.example", Enabled: true, Guest: true},
		identity.DirUser{ID: "id-UUNCLEAR-1", Mail: "uunclear@fixture.example", UPN: "first@fixture.example", Enabled: true},
		identity.DirUser{ID: "id-UUNCLEAR-2", Mail: "second@fixture.example", UPN: "uunclear@fixture.example", Enabled: true})
	idmap := identity.Resolve(users, identity.NewDirectory(dir), nil, identity.Options{})

	many := []string{}
	for i := range 251 {
		many = append(many, "UMANY"+strings.Repeat("X", i%7)+string(rune('A'+i%26))+string(rune('A'+i/26)))
	}
	cases := []struct {
		name    string
		kind    slack.ChannelKind
		members []string
		want    string
	}{
		{"two people", slack.KindDM, []string{"UA", "UB"}, ""},
		{"three people", slack.KindMPIM, []string{"UA", "UB", "UC"}, ""},
		{"two people, one listed twice", slack.KindDM, []string{"UC", "UB", "UC"}, ""},
		{"nobody", slack.KindDM, nil, preflight.DMSkipNoParticipants},
		{"oneself", slack.KindDM, []string{"UA"}, preflight.DMSkipSelf},
		{"oneself, listed twice", slack.KindDM, []string{"UA", "UA"}, preflight.DMSkipSelf},
		{"Slackbot", slack.KindDM, []string{"UA", "USLACKBOT"}, preflight.DMSkipSlackbot},
		{"a bot", slack.KindDM, []string{"UA", "UBOT"}, preflight.DMSkipBot},
		{"an app", slack.KindDM, []string{"UA", "UAPP"}, preflight.DMSkipBot},
		{"a guest, even one with an account", slack.KindDM, []string{"UA", "UGUEST"}, preflight.DMSkipGuest},
		{"someone who left, even with an account", slack.KindDM, []string{"UA", "UGONE"}, preflight.DMSkipDeactivated},
		{"a Slack Connect stranger", slack.KindMPIM, []string{"UA", "UB", "USTRANGER"}, preflight.DMSkipExternal},
		{"someone the export does not list", slack.KindDM, []string{"UA", "UWHO"}, preflight.DMSkipUnknownUser},
		{"someone without an account", slack.KindMPIM, []string{"UA", "UB", "UNOACCOUNT"}, preflight.DMSkipUnmapped},
		{"someone whose account is disabled", slack.KindDM, []string{"UA", "UDISABLED"}, preflight.DMSkipAccountDisabled},
		{"two Slack users of one account", slack.KindDM, []string{"UA", "UTWIN"}, preflight.DMSkipSameAccount},
		{"more people than a chat holds", slack.KindMPIM, many, preflight.DMSkipTooMany},
		{"someone whose account is a guest account", slack.KindDM, []string{"UB", "UINVITED"}, preflight.DMSkipAccountGuest},
		{"someone whose address two accounts answer to", slack.KindDM, []string{"UB", "UUNCLEAR"}, preflight.DMSkipAccountUnclear},
		// Two Slack users of one account, each in a conversation with the
		// same third person: both would land in one chat. Neither goes.
		{"a pair whose accounts another conversation has too", slack.KindDM, []string{"UA", "UC"}, preflight.DMSkipSameChat},
		{"the other conversation with those accounts", slack.KindDM, []string{"UTWIN", "UC"}, preflight.DMSkipSameChat},
	}
	var channels []slack.Channel
	for i, c := range cases {
		channels = append(channels, slack.Channel{ID: "D" + string(rune('A'+i)), Kind: c.kind, Members: c.members})
	}
	channels = append(channels, slack.Channel{ID: "C1", Name: "general", Kind: slack.KindPublic, Members: []string{"UA", "UB"}})
	got := preflight.DirectMessages(channels, users, idmap, true)
	if len(got) != len(cases) {
		t.Fatalf("%d conversations, want %d: a channel is not a direct message", len(got), len(cases))
	}
	for i, c := range cases {
		d := got[i]
		if d.Skip != c.want {
			t.Errorf("%s: skip = %q, want %q", c.name, d.Skip, c.want)
		}
		if (d.Skip == "") != d.Migrates() || (d.Skip != "") != (d.Reason != "") {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
	// Participants are listed once each, in a stable order, and the label
	// names them without their IDs.
	if d := got[2]; d.Participants != 2 || strings.Join(d.Members, ",") != "UB,UC" || d.Label != "Person UB, Person UC" {
		t.Errorf("two people, one listed twice: %+v", d)
	}
	if d := got[16]; !strings.HasSuffix(d.Label, " and 248 more") {
		t.Errorf("label of a large group = %q", d.Label)
	}
	// The stand-in for unmapped authors is never a participant: a
	// conversation with someone unmapped is skipped whatever the policy.
	for _, d := range got {
		for _, id := range d.Members {
			if d.Migrates() && !idmap.Lookup(id).Mapped() {
				t.Errorf("conversation %s migrates with %s, who has no account", d.SlackID, id)
			}
		}
	}

	// What people named a group DM becomes the chat's topic; what Slack
	// generated does not.
	topics := preflight.DirectMessages([]slack.Channel{
		{ID: "G1", Kind: slack.KindMPIM, Name: "mpdm-a--b--c-1", Members: []string{"UA", "UB", "UC"}, Purpose: slack.ChannelValue{Value: "Group messaging with: @a @b @c"}},
		{ID: "G2", Kind: slack.KindMPIM, Name: "mpdm-a--b--c-2", Members: []string{"UA", "UB", "UC"}, Topic: slack.ChannelValue{Value: " Launch crew\x1b[2J "}},
		{ID: "G3", Kind: slack.KindMPIM, Name: "mpdm-a--b--c-3", Members: []string{"UA", "UB", "UC"}, Purpose: slack.ChannelValue{Value: strings.Repeat("x", 400)}},
		{ID: "D9", Kind: slack.KindDM, Members: []string{"UA", "UB"}, Topic: slack.ChannelValue{Value: "ignored for a one-on-one chat"}},
	}, users, idmap, true)
	if topics[0].Topic != "" || topics[1].Topic != "Launch crew" || len(topics[2].Topic) != 200 || topics[3].Topic != "" {
		t.Errorf("topics = %q, %q, %d characters, %q", topics[0].Topic, topics[1].Topic, len(topics[2].Topic), topics[3].Topic)
	}
}

// An export without direct messages, with direct messages asked for.
func TestDirectMessagesMissingFromTheExport(t *testing.T) {
	index := func(v any) *fstest.MapFile {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return &fstest.MapFile{Data: data}
	}
	ex, err := export.OpenFS(fstest.MapFS{
		"users.json":    index([]slack.User{{ID: "U1", Name: "alice", Profile: slack.Profile{Email: "alice@fixture.example"}}}),
		"channels.json": index([]slack.Channel{{ID: "C1", Name: "general", IsGeneral: true}}),
	}, "test export")
	if err != nil {
		t.Fatal(err)
	}
	o := setup(t, "small", withDMs)
	o.Export = ex
	r := run(t, o)
	if !strings.Contains(codes(r.Warnings), "export.no_dms") {
		t.Errorf("warnings = %s", codes(r.Warnings))
	}
	o = setup(t, "small", nil)
	o.Export = ex
	if r := run(t, o); strings.Contains(codes(r.Warnings), "export.no_dms") {
		t.Errorf("direct messages are off, yet: %s", codes(r.Warnings))
	}
}

// A conversation with nothing to import in the selected date range is left
// out, so its chat is never created or touched.
func TestDirectMessagesOutsideTheDateRange(t *testing.T) {
	// The small fixture's direct messages are on its first two days.
	r := run(t, setup(t, "small", func(m *config.Config) {
		withDMs(m)
		m.Content.Since = "2024-03-06"
	}))
	dm := r.DirectMessages
	if dm == nil || dm.OneOnOne+dm.Group != 0 || dm.Skipped != 2 || len(dm.SkippedByReason) != 1 || dm.SkippedByReason[0].Code != preflight.DMSkipNoMessages {
		t.Fatalf("direct messages = %+v", dm)
	}
	if r.Totals.Chats != 0 || r.Totals.Channels == 0 || strings.Contains(codes(r.Warnings), "dm.private") {
		t.Errorf("totals = %+v, warnings = %s", r.Totals, codes(r.Warnings))
	}
	// Half the range: the conversations migrate with what is in it.
	r = run(t, setup(t, "small", func(m *config.Config) {
		withDMs(m)
		m.Content.Since, m.Content.Until = "2024-03-05", "2024-03-05"
	}))
	if dm := r.DirectMessages; dm.OneOnOne != 1 || dm.Group != 1 || dm.Messages != 6 {
		t.Fatalf("direct messages on the second day = %+v", dm)
	}
}

// What a report says about direct messages: how many, and for each one
// that is considered its ID, kind, size and fate. Never who is in it; and
// when they are not migrated, nothing but the count.
func TestDirectMessagesInTheReportNameNobody(t *testing.T) {
	names := []string{}
	for _, p := range fixtures.People("nasty") {
		if len(p.DisplayName) > 3 {
			names = append(names, p.DisplayName)
		}
	}
	for _, on := range []bool{false, true} {
		r := run(t, setup(t, "nasty", func(m *config.Config) { m.Target.DirectMessages.Enabled = on }))
		raw, err := json.Marshal(r.DirectMessages)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if strings.Contains(string(raw), n) {
				t.Errorf("direct messages enabled=%v: the report names %q: %s", on, n, raw)
			}
		}
		if strings.Contains(string(raw), "mpdm-") || strings.Contains(string(raw), `"label"`) || strings.Contains(string(raw), `"topic"`) {
			t.Errorf("direct messages enabled=%v: the report carries a label, a topic or Slack's name for a group DM: %s", on, raw)
		}
		dm := r.DirectMessages
		switch {
		case !on && (dm.InExport != 9 || len(dm.Conversations) != 0 || dm.Skipped != 0 || len(dm.SkippedByReason) != 0 || dm.OneOnOne+dm.Group != 0):
			t.Errorf("direct messages off: the report says more than how many there are: %s", raw)
		case on && len(dm.Conversations) != 9:
			t.Errorf("direct messages on: %d conversations listed", len(dm.Conversations))
		}
		for _, c := range dm.Conversations {
			if c.SlackID == "" || c.Kind == "" || c.Participants == 0 && c.Skip != preflight.DMSkipNoParticipants {
				t.Errorf("conversation = %+v", c)
			}
		}
	}
}

func TestDirectoryAmbiguousKeys(t *testing.T) {
	d := identity.NewDirectory([]identity.DirUser{
		{ID: "1", UPN: "ada@x.example", Mail: "ada@x.example", Enabled: true},
		{ID: "2", UPN: "ben@x.example", Mail: "Shared@x.example", Enabled: true},
		{ID: "3", UPN: "shared@x.example", Mail: "cy@x.example", Enabled: true, Aliases: []string{"SMTP:ada@x.example", "smtp:cy.alias@x.example"}},
	})
	for key, want := range map[string]bool{
		"ada@x.example": true, "shared@x.example": true, "SHARED@X.example": true,
		"ben@x.example": false, "cy@x.example": false, "cy.alias@x.example": false, "1": false, "nobody@x.example": false,
	} {
		if got := d.Ambiguous(key); got != want {
			t.Errorf("Ambiguous(%q) = %v, want %v", key, got, want)
		}
	}
	var none *identity.Directory
	if none.Ambiguous("x") {
		t.Error("a nil directory has ambiguous keys")
	}
}

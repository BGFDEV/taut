package cli_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/cli"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/testutil"
)

// chatRows are a direct message and a group DM as a run reports them: no
// team, and the names of the participants where a channel has its name.
func chatRows(state store.ChannelState, posted int) []store.ChannelProgress {
	return []store.ChannelProgress{
		{SlackID: "D024BE91L", Kind: store.KindDM, TargetName: "Ada Abbott, Ben Banerjee", State: state, Posts: 120, Posted: posted, Files: 3, FilesDone: 3},
		{SlackID: "G07QX3M2K", SlackName: "mpdm-ada.abbott--ben.banerjee--chloe.castro-1", Kind: store.KindMPIM,
			TargetName: "Ada Abbott, Ben Banerjee, Chloé Castro", State: state, Posts: 40, Posted: min(posted, 40)},
	}
}

// Importing private conversations is said out loud before anything is
// written: which go where, which do not, and why.
func TestPreflightDirectMessages(t *testing.T) {
	e := newEnv(t, "medium")
	e.init("--include-direct-messages")
	out, errOut, code := e.run("preflight")
	if code != fail.ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	noSecrets(t, out, errOut)
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"Direct messages", "These are private conversations",
		"visible to its participants while the import runs",
		"the chat the two people already have in Teams",
		"Files are not moved", "Thread replies become ordinary messages",
		"a participant's Slack account is deactivated",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the report does not say %q", want)
		}
	}
	testutil.Golden(t, "preflight_dms_plain", out)

	// --json has every conversation, with the reason for each that is left
	// out, and no message text.
	out, errOut, code = e.run("preflight", "--json")
	if code != fail.ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	var rep preflight.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	dm := rep.DirectMessages
	if dm == nil || !dm.Enabled || dm.OneOnOne+dm.Group == 0 || dm.Skipped == 0 || len(dm.Conversations) != dm.InExport {
		t.Fatalf("directMessages = %+v", dm)
	}
	for _, c := range dm.Conversations {
		if c.SlackID == "" || c.Kind == "" || c.Participants < 2 || (c.Skip == "") != (c.Reason == "") {
			t.Errorf("conversation = %+v", c)
		}
	}
	// Per conversation: which, of what kind, how many people, and why not.
	// Never who: the names of the people in a private conversation are in
	// `taut map`, on the operator's screen, and nowhere else.
	var doc struct {
		DirectMessages struct {
			Conversations []map[string]any `json:"conversations"`
		} `json:"directMessages"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"slackId": true, "kind": true, "participants": true, "skip": true, "reason": true}
	for _, c := range doc.DirectMessages.Conversations {
		for k := range c {
			if !allowed[k] {
				t.Errorf("a conversation in --json carries %q: %v", k, c)
			}
		}
	}
	for _, name := range []string{"Ada", "Abbott", "mpdm-"} {
		if i := strings.Index(out, `"directMessages"`); i >= 0 {
			j := strings.Index(out[i:], `"conversations"`)
			if j >= 0 && strings.Contains(out[i+j:i+j+min(len(out)-i-j, 4000)], name) {
				t.Errorf("the conversations in --json name %q", name)
			}
		}
	}

	// With direct messages off the report counts them and says no more.
	off := newEnv(t, "medium")
	off.init()
	out, errOut, code = off.run("preflight", "--json")
	if code != fail.ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	var offRep preflight.Report
	if err := json.Unmarshal([]byte(out), &offRep); err != nil {
		t.Fatal(err)
	}
	if d := offRep.DirectMessages; d == nil || d.Enabled || d.InExport == 0 || len(d.Conversations) != 0 || d.Messages != 0 || len(d.SkippedByReason) != 0 {
		t.Errorf("direct messages off: %+v", d)
	}
	if rep.Totals.Chats != dm.OneOnOne+dm.Group || rep.Totals.ChatMessages != dm.Messages {
		t.Errorf("totals = %+v, direct messages = %+v", rep.Totals, dm)
	}

	// `taut auth` asks for what direct messages really use: two more
	// Graph permissions, and nothing more of Slack.
	out, errOut, code = e.run("auth", "--json")
	if code != fail.ExitOK {
		t.Fatalf("auth with direct messages on: exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out, "ChatMember.ReadWrite.All") || strings.Contains(out, "im:history") {
		t.Errorf("auth asks for a permission direct messages do not use:\n%s", out)
	}
	for _, want := range []string{"Chat.Create", "Chat.ReadBasic.All"} {
		if !strings.Contains(out, want) {
			t.Errorf("auth does not mention %s", want)
		}
	}

	// The permissions the feature needs are now required.
	for _, p := range rep.Permissions {
		if (p.Name == "Chat.Create" || p.Name == "Chat.ReadBasic.All") && string(p.State) != "granted" {
			t.Errorf("permission %s is %s with direct messages on", p.Name, p.State)
		}
	}
}

// The config file refuses direct messages where they cannot be migrated,
// and says what to do instead.
func TestDirectMessagesRefusedByTheConfig(t *testing.T) {
	e := newEnv(t, "medium")
	e.init("--include-direct-messages")
	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(raw), "strategy: modern", "strategy: legacy", 1)
	if legacy == string(raw) {
		t.Fatalf("the config file does not state the load strategy:\n%s", raw)
	}
	if err := os.WriteFile(e.config, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := e.run("preflight")
	flat := strings.Join(strings.Fields(errOut), " ")
	if code == fail.ExitOK || !strings.Contains(flat, "target.directMessages.enabled") || !strings.Contains(flat, "set load.strategy to modern") {
		t.Errorf("legacy with direct messages: exit %d\n%s", code, errOut)
	}
}

// `taut migrate` says that private conversations are about to be imported
// even when nobody is asked: with --yes the warning is printed.
func TestMigrateSaysItImportsPrivateConversations(t *testing.T) {
	enable := func(e *dashboardEnv) {
		t.Helper()
		raw, err := os.ReadFile(e.config)
		if err != nil {
			t.Fatal(err)
		}
		on := strings.Replace(string(raw), "directMessages:\n    enabled: false", "directMessages:\n    enabled: true", 1)
		if on == string(raw) {
			t.Fatalf("the config file does not state directMessages.enabled:\n%s", raw)
		}
		if err := os.WriteFile(e.config, []byte(on), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := newDashboardEnv(t)
	e.noRun = true // nothing imported yet; the stand-in accepts no new run

	// Without direct messages there is nothing to say about them.
	out, _, _ := e.run("migrate", "--yes")
	if strings.Contains(out, "private conversations") {
		t.Errorf("direct messages are off, yet migrate mentions them:\n%s", out)
	}

	enable(e)
	// It cannot ask, and --yes was not given: it does not start.
	_, errOut, code := e.run("migrate")
	if code != fail.ExitBlockers || !strings.Contains(errOut, "taut migrate --yes") {
		t.Errorf("without --yes: exit %d\n%s", code, errOut)
	}
	// With --yes it proceeds, and says what it is about to do first.
	out, errOut, _ = e.run("migrate", "--yes")
	noSecrets(t, out, errOut)
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{"Direct messages", "These are private conversations", "visible to its participants while the import runs", "Tell the people concerned before you start"} {
		if !strings.Contains(flat, want) {
			t.Errorf("migrate --yes does not say %q:\n%s", want, out)
		}
	}
	// --json stays one document, and the document says it too: with --yes
	// nobody was asked, and with --json nothing else is printed.
	e.accept = true
	for _, args := range [][]string{{"migrate", "--yes", "--json"}, {"migrate", "--yes", "--json", "--detach"}, {"migrate", "--yes", "--json", "--no-preflight"}} {
		out, errOut, code := e.run(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s\n%s", args, code, out, errOut)
		}
		var doc struct {
			DirectMessages *struct{ Code, Notice, Next string } `json:"directMessages"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v did not print one JSON document: %v\n%s", args, err, out)
		}
		if dm := doc.DirectMessages; dm == nil || dm.Code != "dm.private" || !strings.Contains(dm.Notice, "These are private conversations") ||
			!strings.Contains(dm.Notice, "visible to its participants while the import runs") || !strings.Contains(dm.Next, "target.directMessages.enabled") {
			t.Errorf("%v: the document does not carry the notice about direct messages:\n%s", args, out)
		}
	}

	// Skipping the preflight does not skip the question: without --yes
	// nothing starts, and with it the notice is printed all the same.
	created := e.created
	_, errOut, code = e.run("migrate", "--no-preflight")
	if code != fail.ExitBlockers || !strings.Contains(errOut, "Importing direct messages needs confirmation") || e.created != created {
		t.Errorf("--no-preflight without --yes: exit %d, %d runs started\n%s", code, e.created-created, errOut)
	}
	out, errOut, code = e.run("migrate", "--no-preflight", "--yes")
	flat = strings.Join(strings.Fields(out), " ")
	if code != 0 || !strings.Contains(flat, "These are private conversations") || e.created != created+1 {
		t.Errorf("--no-preflight --yes: exit %d\n%s\n%s", code, out, errOut)
	}
}

// Direct messages that are not migrated do not leave the operator's
// machine: the copy of the export that is uploaded has none of them. With
// direct messages on, the export goes up as it is.
func TestExportIsUploadedWithoutDirectMessagesUnlessTheyAreMigrated(t *testing.T) {
	entries := func(t *testing.T, raw []byte) (names []string, private int) {
		t.Helper()
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("what was uploaded is not a ZIP: %v", err)
		}
		for _, f := range zr.File {
			names = append(names, f.Name)
			base := f.Name[strings.LastIndex(f.Name, "/")+1:]
			top, _, _ := strings.Cut(f.Name, "/")
			if base == "dms.json" || base == "mpims.json" || strings.HasPrefix(top, "mpdm-") || (strings.HasPrefix(top, "D") && strings.ToUpper(top) == top && strings.Contains(f.Name, "/")) {
				private++
			}
		}
		return names, private
	}
	original, err := os.ReadFile(filepath.Join(newEnv(t, "medium").dir, "medium.zip"))
	if err != nil {
		t.Fatal(err)
	}
	all, private := entries(t, original)
	if private == 0 {
		t.Fatal("the fixture has no direct messages")
	}

	e := newDashboardEnv(t)
	e.noRun, e.accept = true, true
	if out, errOut, code := e.run("migrate", "--yes", "--json"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	got, left := entries(t, e.uploaded)
	if left != 0 {
		t.Errorf("direct messages are off, yet %d of their files were uploaded: %v", left, got)
	}
	if len(got) != len(all)-private {
		t.Errorf("%d entries uploaded, want the export's %d without its %d of direct messages", len(got), len(all), private)
	}
	// No copy is left behind.
	if leftovers, _ := filepath.Glob(filepath.Join(os.TempDir(), "taut-export-*.zip")); len(leftovers) != 0 {
		t.Errorf("the copy without direct messages was not removed: %v", leftovers)
	}

	on := newDashboardEnv(t)
	on.noRun, on.accept = true, true
	raw, err := os.ReadFile(on.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(on.config, []byte(strings.Replace(string(raw), "directMessages:\n    enabled: false", "directMessages:\n    enabled: true", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := on.run("migrate", "--yes", "--json"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !bytes.Equal(on.uploaded, original) {
		t.Error("direct messages are on, yet the export was not uploaded as it is")
	}
}

// The dashboard shows a direct message by the names of its participants,
// not as a channel, and counts chats apart from channels.
func TestDashboardPlainDirectMessages(t *testing.T) {
	e := newDashboardEnv(t)
	p := e.view.Run.Progress
	p.Channels = append(p.Channels, chatRows(store.ChannelMigrating, 60)...)
	p.Posts, p.Posted = p.Posts+160, p.Posted+100
	out := e.ok("dashboard")
	// The plain snapshot has no headings; the full-screen dashboard groups
	// them under "Direct messages" (see internal/tui/dashboard).
	if !strings.Contains(out, "2 direct-message conversations") || !strings.Contains(out, "Ada Abbott, Ben Banerjee") ||
		strings.Contains(out, "mpdm-") || strings.Contains(out, "#Ada") {
		t.Errorf("the dashboard does not list the direct messages as such:\n%s", out)
	}
	testutil.Golden(t, "dashboard_plain_dms", out)
}

// Finalizing counts the chats of direct messages apart from the channels,
// and says that it opens nothing to anybody there.
func TestFinalizeDirectMessages(t *testing.T) {
	withChats := func(t *testing.T) *finalizeEnv {
		e := newFinalizeEnv(t, store.RunCompleted)
		p := e.state.Progress
		p.Channels = append(p.Channels, chatRows(store.ChannelMigrating, 120)...)
		p.Posts, p.Posted = p.Posts+160, p.Posted+160
		return e
	}
	e := withChats(t)
	out, errOut, code := e.run("finalize", "--yes")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	noSecrets(t, out, errOut)
	if !strings.Contains(out, "2 chats out of migration mode") {
		t.Errorf("the summary does not count the chats:\n%s", out)
	}
	testutil.Golden(t, "finalize_plain_dms", testutil.Stable(out))
	if reqs, _ := e.requests(); len(reqs) != 1 || len(reqs[0].Channels) != 0 {
		t.Errorf("requests = %+v, want one for everything", reqs)
	}

	// All of one kind in one request.
	for only, want := range map[string]string{
		"direct-messages": "D024BE91L,G07QX3M2K",
		"channels":        "C024BE91L,C07QX3M2K,C09ZZ81AB",
	} {
		e = withChats(t)
		if out, errOut, code := e.run("finalize", "--yes", "--only", only, "--json"); code != 0 {
			t.Fatalf("--only %s: exit %d:\n%s\n%s", only, code, out, errOut)
		}
		if reqs, _ := e.requests(); len(reqs) != 1 || strings.Join(reqs[0].Channels, ",") != want {
			t.Errorf("--only %s: requests = %+v, want %s", only, reqs, want)
		}
	}

	// One direct message, by its Slack ID.
	e = withChats(t)
	if out, errOut, code := e.run("finalize", "--yes", "--channel", "D024BE91L", "--json"); code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	if reqs, _ := e.requests(); len(reqs) != 1 || strings.Join(reqs[0].Channels, ",") != "D024BE91L" {
		t.Errorf("requests = %+v", reqs)
	}

	// Never by the names of the people in it, nor by the name Slack made
	// of them; and the two ways of selecting do not mix.
	e = withChats(t)
	for _, args := range [][]string{
		{"--channel", "Ada Abbott, Ben Banerjee"},
		{"--channel", "mpdm-ada.abbott--ben.banerjee--chloe.castro-1"},
		{"--only", "everything"},
		{"--only", "channels", "--channel", "general"},
	} {
		_, errOut, code := e.run(append([]string{"finalize", "--yes"}, args...)...)
		if code != fail.ExitUsage {
			t.Errorf("finalize %v: exit %d, want %d:\n%s", args, code, fail.ExitUsage, errOut)
		}
	}
	if reqs, _ := e.requests(); len(reqs) != 0 {
		t.Errorf("a refused selection sent %d requests", len(reqs))
	}

	// A migration without direct messages has none to select.
	plain := newFinalizeEnv(t, store.RunCompleted)
	if _, errOut, code := plain.run("finalize", "--yes", "--only", "direct-messages"); code != fail.ExitUsage || !strings.Contains(errOut, "no direct messages") {
		t.Errorf("--only direct-messages without any: exit %d\n%s", code, errOut)
	}
}

// `taut status` shows a direct message by its participants and counts
// chats apart from channels.
func TestStatusDirectMessages(t *testing.T) {
	e := newFinalizeEnv(t, store.RunRunning)
	p := e.state.Progress
	p.Channels[0].Posted = 2000
	p.Channels = append(p.Channels, chatRows(store.ChannelMigrating, 60)...)
	p.Posts, p.Posted = p.Posts+160, p.Posted-2200+100
	out, errOut, code := e.run("status")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"3 channels, 2 direct-message conversations", "Ada Abbott, Ben Banerjee", "#general"} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not show %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#Ada") || strings.Contains(out, "mpdm-") {
		t.Errorf("status shows a direct message as a channel:\n%s", out)
	}
}

// A chat share that is not a share stops the egress proxy at start; it is
// not replaced by the default, and 1 is a share.
func TestEgressProxyRefusesABadChatShare(t *testing.T) {
	for _, v := range []string{"0", "-0.5", "1.5", "2", "half", "NaN", "+Inf"} {
		var out, errb bytes.Buffer
		code := cli.Main(context.Background(), []string{"sidecar", "egress-proxy"}, strings.NewReader(""), &out, &errb,
			[]string{"TERM=dumb", "TAUT_GRAPH_CHAT_SHARE=" + v, "TAUT_PROXY_ADDR=127.0.0.1:0"})
		if code != fail.ExitUsage || !strings.Contains(errb.String(), "TAUT_GRAPH_CHAT_SHARE") {
			t.Errorf("chat share %q: exit %d, want %d and the variable named:\n%s", v, code, fail.ExitUsage, errb.String())
		}
	}
}

// The chats of a run that failed or was cancelled stay in migration mode
// unless something ends it. `taut finalize --only direct-messages` does,
// without pretending the import is finalized; anything else is refused as
// before, and the refusal says what can be done about the chats.
func TestFinalizeReleasesTheChatsOfAnAbandonedRun(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunFailed, store.RunCancelled} {
		abandoned := func(t *testing.T) *finalizeEnv {
			e := newFinalizeEnv(t, status)
			p := e.state.Progress
			p.Channels = append(p.Channels, chatRows(store.ChannelMigrating, 60)...)
			p.Posts, p.Posted = p.Posts+160, p.Posted+100
			return e
		}
		t.Run(string(status), func(t *testing.T) {
			// Everything: refused, with the way out for the chats.
			e := abandoned(t)
			_, errOut, code := e.run("finalize", "--yes")
			flat := strings.Join(strings.Fields(errOut), " ")
			if code != fail.ExitBlockers || !strings.Contains(flat, "2 chats of direct messages are still in migration mode") ||
				!strings.Contains(flat, "taut finalize --only direct-") { // the line wraps at the hyphen
				t.Errorf("finalize on a %s run: exit %d\n%s", status, code, errOut)
			}
			for _, args := range [][]string{{"--only", "channels"}, {"--channel", "general"}, {"--channel", "general", "--channel", "D024BE91L"}} {
				if _, errOut, code := e.run(append([]string{"finalize", "--yes"}, args...)...); code != fail.ExitBlockers {
					t.Errorf("finalize %v on a %s run: exit %d\n%s", args, status, code, errOut)
				}
			}
			if reqs, _ := e.requests(); len(reqs) != 0 {
				t.Errorf("refused requests reached the control plane: %+v", reqs)
			}

			// It cannot be undone, so it is not done unasked.
			if _, errOut, code := e.run("finalize", "--only", "direct-messages"); code != fail.ExitBlockers || !strings.Contains(errOut, "--only direct-messages --yes") {
				t.Errorf("without --yes: exit %d\n%s", code, errOut)
			}
			if reqs, _ := e.requests(); len(reqs) != 0 {
				t.Errorf("an unconfirmed release sent %d requests", len(reqs))
			}

			out, errOut, code := e.run("finalize", "--yes", "--only", "direct-messages")
			if code != 0 {
				t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
			}
			noSecrets(t, out, errOut)
			if !strings.Contains(out, "Chats out of migration mode") || !strings.Contains(out, "2 chats") || strings.Contains(out, "finalized") {
				t.Errorf("the report:\n%s", out)
			}
			if status == store.RunCancelled {
				testutil.Golden(t, "finalize_release_plain", testutil.Stable(out))
			}
			if reqs, _ := e.requests(); len(reqs) != 1 || strings.Join(reqs[0].Channels, ",") != "D024BE91L,G07QX3M2K" || reqs[0].Retain {
				t.Errorf("requests = %+v", reqs)
			}
			// Again: nothing left to do, and nothing asked.
			out, _, code = e.run("finalize", "--yes", "--only", "direct-messages")
			if code != 0 || !strings.Contains(out, "No chat of this run is in migration mode") {
				t.Errorf("a second time: exit %d\n%s", code, out)
			}
			if reqs, _ := e.requests(); len(reqs) != 1 {
				t.Errorf("a second time sent another request: %+v", reqs)
			}

			// One chat by its Slack ID, as JSON.
			e = abandoned(t)
			out, errOut, code = e.run("finalize", "--yes", "--channel", "D024BE91L", "--json")
			if code != 0 {
				t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
			}
			var res struct {
				Changed  bool `json:"changed"`
				Released int  `json:"released"`
				Still    int  `json:"stillInMigrationMode"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Changed || res.Released != 1 || res.Still != 0 {
				t.Errorf("--json: %v %+v\n%s", err, res, out)
			}

			// A release that fails is a failure, and says which.
			e = abandoned(t)
			e.releaseFails = true
			out, errOut, code = e.run("finalize", "--yes", "--only", "direct-messages")
			if code != fail.ExitFailure || !strings.Contains(out, "still in migration mode") || !strings.Contains(out, "release:D024BE91L") {
				t.Errorf("a failed release: exit %d\n%s\n%s", code, out, errOut)
			}
		})
	}
}

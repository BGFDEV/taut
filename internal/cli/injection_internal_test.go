package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/dashboard"
	"github.com/bgfdev/taut/internal/tui/mapping"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// hostile are the things text Taut did not write can carry to take over a
// terminal. Runes are built from numbers so this file holds none of them.
var hostile = []struct{ name, text string }{
	{"window title (OSC 0)", "\x1b]0;x\x07"},
	{"clear screen (CSI 2J)", "\x1b[2J"},
	{"clipboard write (OSC 52)", "\x1b]52;c;AAAA\x07"},
	{"device control string (DCS)", "\x1bPq#0;2;0;0;0\x1b\\"},
	{"unterminated OSC", "\x1b]0;never ended"},
	{"bare carriage return", "\r"},
	{"line breaks and tab", "\r\n\t\n"},
	{"C1 CSI as a rune", string(rune(0x9b)) + "2J"},
	{"C1 CSI as a raw byte", "\x9b2J"},
	{"delete and bell", "\x7f\x07\x00"},
	{"bidi override (U+202E)", string(rune(0x202e))},
	{"zero width and soft hyphen", string([]rune{0x200b, 0xad, 0x2066, 0x206a, 0xfeff})},
}

// sgr is the one kind of escape sequence the renderer itself emits: colors
// and weights. Only these are removed before the output is checked. (The
// usual helper, testutil.Plain, strips every escape sequence, which would
// remove exactly what this test is looking for.)
var sgr = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// assertSafe fails if out could do anything to a terminal but print text
// and break lines.
func assertSafe(t *testing.T, where, out string) {
	t.Helper()
	out = sgr.ReplaceAllString(out, "")
	if !utf8.ValidString(out) {
		t.Errorf("%s: output is not valid UTF-8: %q", where, out)
		return
	}
	for _, r := range out {
		switch {
		case r == '\n':
		case r < 0x20, r == 0x7f:
			t.Errorf("%s: control character %U in %q", where, r, out)
			return
		case r >= 0x80 && r <= 0x9f:
			t.Errorf("%s: C1 control %U in %q", where, r, out)
			return
		case unicode.Is(unicode.Cf, r):
			t.Errorf("%s: invisible format character %U in %q", where, r, out)
			return
		}
	}
}

// eachString calls fn for every string reachable from v.
func eachString(v reflect.Value, fn func(string)) {
	switch v.Kind() {
	case reflect.String:
		fn(v.String())
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			eachString(v.Elem(), fn)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				eachString(v.Field(i), fn)
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return // raw JSON is not text to draw
		}
		for i := range v.Len() {
			eachString(v.Index(i), fn)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			eachString(k, fn)
			eachString(v.MapIndex(k), fn)
		}
	}
}

func assertSafeValue(t *testing.T, where string, v any) {
	t.Helper()
	n := 0
	eachString(reflect.ValueOf(v), func(s string) {
		n++
		if strings.Contains(s, "\n") && !strings.Contains(where, "error text allowed") {
			// Only error text keeps line breaks; see sanitizeRun.
			s = strings.ReplaceAll(s, "\n", "")
		}
		assertSafe(t, where, s)
	})
	if n == 0 {
		t.Errorf("%s: no strings were checked", where)
	}
}

// hostileDashboard is a dashboard snapshot in which every string the controller relays
// (from the Slack export, from Microsoft Graph, from the bus) carries p.
func hostileDashboard(p string) controller.DashboardView {
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	x := func(s string) string { return s[:2] + p + s[2:] }
	return controller.DashboardView{
		At: at,
		Run: controller.RunView{
			ID: "01JCM4Q8ZK7W3N5T2R6V9XYB0D", Name: x("acme-migration"), Status: store.RunRunning, CreatedAt: at.Add(-time.Hour),
			Progress: &store.Progress{
				Stages: []store.StageProgress{{Stage: store.StageLoad, Lanes: 2, Leased: 1, Batches: 10, BatchesDone: 4}},
				Channels: []store.ChannelProgress{
					{SlackID: x("C0000001"), SlackName: x("general"), TargetName: x("General"), Team: x("Engineering"),
						State: store.ChannelMigrating, Posts: 100, Posted: 40, Failed: 2, Active: true},
					{SlackID: x("C0000002"), SlackName: x("incidents"), TargetName: x("incidents"), Team: x("Engineering"),
						State: store.ChannelMigrating, Posts: 50, Posted: 10, LaneFailed: true, LastError: x("Graph refused the message: 403 Forbidden")},
					// A direct message is shown by the names of its
					// participants, which come from the export too.
					{SlackID: x("D0000003"), TargetName: x("Ada Abbott, Ben Banerjee"), Kind: store.KindDM,
						State: store.ChannelMigrating, Posts: 20, Posted: 5, Active: true},
				},
				Problems: []store.LaneProblem{
					{LaneID: x("load:C0000002"), Stage: x("load"), Status: x(string(store.LaneFailed)), Attempts: 5,
						Error: x("Graph refused the message: 403 Forbidden")},
					{LaneID: "files:" + x("C0000001"), Stage: store.StageFiles, Status: string(store.LaneOpen), Attempts: 1,
						NotBefore: at.Add(10 * time.Second), Error: x("upload session expired")},
				},
				Posts: 150, Posted: 50, Failed: 2,
			},
		},
		Cluster: controller.ClusterView{
			Workers: []store.Holder{{Holder: x("worker-a1f3"), Lanes: 2, Stages: []string{x("files"), x("load")}, LeaseExpires: at.Add(30 * time.Second)}},
			Budgets: []budget.KeyStatus{
				{Key: budget.Key{Tenant: x("9f1c2d3e-0000-4000-8000-acme"), Scope: x("graph")}, TotalRPS: 40, ShareRPS: 20, Holders: []string{x("egress-proxy-a"), x("egress-proxy-b")}},
				{Key: budget.Key{Tenant: x("another-tenant-0000-4000"), Scope: x("sharepoint")}, TotalRPS: 10, ShareRPS: 10, Holders: []string{x("egress-proxy-a")}},
			},
			Throttles: []controller.ThrottleView{
				{Tenant: x("9f1c2d3e-0000-4000-8000-acme"), Scope: x("graph"), Holder: x("egress-proxy-a"), At: at.Add(-time.Minute), Until: at.Add(time.Minute)},
			},
		},
	}
}

// frames renders a screen inside the shell at a wide and a narrow size.
func frames(scr shell.Screen, command string) map[string]string {
	out := map[string]string{}
	for name, size := range map[string]tea.WindowSizeMsg{"wide": {Width: 140, Height: 50}, "narrow": {Width: 80, Height: 24}} {
		var m tea.Model = shell.New(scr, shell.Options{Theme: theme.Default(), Command: command, Context: "acme", LockTheme: true})
		m, _ = m.Update(size)
		out[name] = m.View().Content
	}
	return out
}

// Text that is not Taut's reaches the operator's terminal by three routes:
// from the controller through the client, from the Slack export through map and
// preflight, and from the audit log. Whatever it carries, what is drawn can only
// print text.
func TestHostileTextCannotDriveTheTerminal(t *testing.T) {
	th := theme.Default()
	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.text

			// --- from the controller ------------------------------------------------
			raw := hostileDashboard(p)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/dashboard"):
					_ = json.NewEncoder(w).Encode(raw)
				case strings.HasSuffix(r.URL.Path, "/finalize"):
					_ = json.NewEncoder(w).Encode(controller.FinalizeView{Run: raw.Run, Channels: 2})
				case strings.HasSuffix(r.URL.Path, "/audit/verify"):
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "problem": "stream load:C1" + p + " is broken"})
				case strings.HasSuffix(r.URL.Path, "/cancel"):
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": fail.New("run.co"+p+"de", "The run cannot be "+p+"cancelled",
						"Graph said: "+p+"\n  no", "Run `taut "+p+"dashboard`.")})
				default:
					_ = json.NewEncoder(w).Encode(raw.Run)
				}
			}))
			defer srv.Close()
			client, ctx := controller.NewClient(srv.URL, ""), context.Background()

			view, err := client.Dashboard(ctx, "r1")
			if err != nil {
				t.Fatal(err)
			}
			assertSafeValue(t, "DashboardView from the client", view)
			if len(view.Cluster.Budgets) != 2 || len(view.Cluster.Budgets[0].Holders) != 2 || len(view.Run.Progress.Channels) != 3 {
				t.Fatalf("sanitizing changed the shape of the view: %+v", view.Cluster)
			}
			run, err := client.Run(ctx, "r1")
			if err != nil {
				t.Fatal(err)
			}
			assertSafeValue(t, "RunView from the client", run)
			latest, err := client.LatestRun(ctx, "acme")
			if err != nil {
				t.Fatal(err)
			}
			assertSafeValue(t, "RunView from LatestRun", latest)
			waited, err := client.Wait(ctx, "r1", time.Millisecond, func(controller.RunView) bool { return false })
			if err != nil {
				t.Fatal(err)
			}
			assertSafeValue(t, "RunView from Wait", waited)
			finalized, err := client.Finalize(ctx, "r1", controller.FinalizeRequest{})
			if err != nil {
				t.Fatal(err)
			}
			assertSafeValue(t, "FinalizeView from the client", finalized)
			if _, problem, err := client.VerifyAuditLog(ctx, "r1"); err != nil || problem == "" {
				t.Fatalf("verify: %q, %v", problem, err)
			} else {
				assertSafe(t, "audit log verification problem", problem)
			}
			fe := fail.From(client.Cancel(ctx, "r1"))
			if fe == nil || fe.What == "" || fe.Why == "" || fe.Next == "" {
				t.Fatalf("cancel = %v", fe)
			}
			assertSafeValue(t, "error envelope (error text allowed)", fe.Code+"\n"+fe.What+"\n"+fe.Why+"\n"+fe.Next)

			// The plain outputs of dashboard, migrate and finalize are drawn from these
			// views; dashboard's includes the run as migrate and status draw it.
			assertSafe(t, "dashboard, plain", dashboardPlain(th, "acme", view, 100))
			src := dashboardSource{client: client, run: "r1"}
			for size, frame := range frames(dashboard.New(src, dashboard.Options{Initial: &view}), "dashboard") {
				assertSafe(t, "dashboard, full screen, "+size, frame)
				if !strings.Contains(frame, "Budgets") && size == "wide" {
					t.Errorf("the cluster pane was not drawn:\n%s", frame)
				}
			}

			// --- from the Slack export -------------------------------------
			f, _ := mapFixture(t)
			channels := []slack.Channel{
				{ID: "C1", Name: "general", Kind: slack.KindPublic, IsGeneral: true},
				{ID: "C2" + p, Name: "de" + p + "sign", Kind: slack.KindPublic},
				{ID: "C3", Name: "sales-" + p + "emea", Kind: slack.KindPublic, IsArchived: true},
				{ID: "C4", Name: "tmp-" + p + "old", Kind: slack.KindPublic},
				{ID: "G1", Name: "le" + p + "ads", Kind: slack.KindPrivate},
			}
			ed := mapping.NewEditor(f.loaded, channels, nil, map[string]int{"C2" + p: 3})
			outcome := mapOutcome{file: f}
			assertSafe(t, "map, plain", mapPlain(th, "acme", ed.View(), nil, outcome, 100))
			assertSafeValue(t, "map view (the --json document)", ed.View())
			for size, frame := range frames(mapping.NewScreen(ed, mapping.Options{File: f.name}), "map") {
				assertSafe(t, "map, full screen, "+size, frame)
			}
			// An edit names the channel in its change list and its messages.
			shown := ed.View().Teams[0].Channels[1].SlackName
			if err := ed.Exclude(shown); err != nil {
				t.Fatalf("the channel could not be edited by the name map shows, %q: %v", shown, err)
			}
			outcome.edits, outcome.dirty, outcome.retry = true, true, "taut map --exclude x --yes"
			assertSafe(t, "map, plain, after an edit", mapPlain(th, "acme", ed.View(), ed.Unsaved(), outcome, 100))
			assertSafeValue(t, "map changes", ed.Unsaved())
			for _, err := range []error{ed.Rename(shown, "Studio"), ed.Include("le"+p+"ads", ""), ed.Exclude("no" + p + "ne")} {
				if fe := fail.From(err); fe != nil {
					assertSafeValue(t, "map refusal (error text allowed)", fe.What+"\n"+fe.Why+"\n"+fe.Next)
				}
			}

			lay := preflight.Layout(f.loaded, channels, nil)
			rep := &preflight.Report{
				Config: "acme", Verdict: preflight.VerdictBlocked, Teams: lay.Teams, Skipped: lay.Skipped,
				Blockers: lay.Blockers, Warnings: append(lay.Warnings, preflight.Finding{Code: "export.day_unreadable", Message: "Part of #de" + p + "sign could not be read: " + p}),
				Identity: preflight.IdentityReport{Checked: true, Unmapped: 1, Policy: "former-member", Account: "former@acme.example",
					UnmappedUsers: []preflight.UnmappedUser{{SlackID: "U1" + p, Name: "Mal" + p + "lory", Email: "m" + p + "@evil.example", Reason: "no_match", Detail: "no Entra user has this address", Messages: 4}}},
			}
			assertSafe(t, "preflight, plain", preflightDocument(rep).Plain(th, 100))
		})
	}
}

// preflight says so when the export holds a channel or a person whose name
// cannot have been typed.
func TestPreflightFlagsControlCharactersInTheExport(t *testing.T) {
	f, _ := mapFixture(t)
	esc := "\x1b[2J"
	lay := preflight.Layout(f.loaded, []slack.Channel{
		{ID: "C1", Name: "general", Kind: slack.KindPublic, IsGeneral: true},
		{ID: "C2", Name: "de" + esc + "sign", Kind: slack.KindPublic}, // migrates: a blocker
		{ID: "C3", Name: "tmp-" + esc, Kind: slack.KindPublic},        // excluded: a warning
		{ID: "C4", Name: "dev" + string(rune(0x200d)) + "ops", Kind: slack.KindPublic},
	}, nil)
	if len(lay.Blockers) != 1 || lay.Blockers[0].Code != "export.channel_control_chars" || lay.Blockers[0].Next == "" ||
		!strings.Contains(lay.Blockers[0].Message, "#design (C2)") {
		t.Errorf("blockers = %+v", lay.Blockers)
	}
	if len(lay.Warnings) != 1 || lay.Warnings[0].Code != "export.channel_control_chars" || !strings.Contains(lay.Warnings[0].Message, "not migrating") {
		t.Errorf("warnings = %+v", lay.Warnings)
	}
	// The name itself is untouched: patterns in the config file match it as
	// the export has it.
	if got := lay.Teams[0].Channels[1].SlackName; got != "de"+esc+"sign" {
		t.Errorf("Layout changed the channel's name to %q", got)
	}
}

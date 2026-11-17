package cli_test

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/auditlog/auditlogtest"
)

const auditLogRunID = "run-2026-11-14-a1b2c3"

// auditLogEnv is a config file pointing at a stand-in controller that serves the
// sample audit log over the real HTTP API, so these tests cover the command
// end to end: flags, the client's query encoding, paging and output.
type auditLogEnv struct {
	*env
	fake *auditlogtest.Fake
	// noRun makes the stand-in report that the config file has no run.
	noRun bool
	here  string
}

func newAuditLogEnv(t *testing.T) *auditLogEnv {
	t.Helper()
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	e := &auditLogEnv{env: newEnv(t, "medium"), fake: auditlogtest.New(auditlogtest.Sample()), here: here}
	e.init()

	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	notFound := func(w http.ResponseWriter) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fail.New("run.not_found", "No such run", "", "Run `taut migrate` to start one.")})
	}
	run := controller.RunView{
		ID: auditLogRunID, Name: "fixture", Status: store.RunCompleted,
		CreatedAt: auditlogtest.Start, UpdatedAt: auditlogtest.Start,
		Progress: &store.Progress{Channels: []store.ChannelProgress{
			{SlackID: "C024BE91L", SlackName: "general"},
			{SlackID: "C07QX3M2K", SlackName: "random"},
			{SlackID: "D024BE91L", Kind: store.KindDM, TargetName: "Ada Abbott, Ben Banerjee"},
			{SlackID: "G07QX3M2K", SlackName: "mpdm-ada--ben--cy-1", Kind: store.KindMPIM, TargetName: "Ada Abbott, Ben Banerjee, Cy Castro"},
		}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		if e.noRun || r.URL.Query().Get("name") != "fixture" {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != auditLogRunID {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, run)
	})
	mux.HandleFunc("GET /v1/runs/{id}/audit", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := store.LogFilter{Stream: q.Get("stream"), Op: q.Get("op"), Outcome: q.Get("outcome"), Contains: q.Get("q")}
		f.Since, _ = time.Parse(time.RFC3339Nano, q.Get("since"))
		f.Until, _ = time.Parse(time.RFC3339Nano, q.Get("until"))
		f.Limit, _ = strconv.Atoi(q.Get("limit"))
		f.After.Stream = q.Get("afterStream")
		f.After.Seq, _ = strconv.ParseInt(q.Get("afterSeq"), 10, 64)
		page, err := e.fake.AuditLog(r.Context(), f)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fail.From(err)})
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /v1/runs/{id}/audit/verify", func(w http.ResponseWriter, r *http.Request) {
		ok, problem, err := e.fake.Verify(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fail.From(err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "problem": problem})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	const defaultURL = "http://localhost:7070"
	if !strings.Contains(string(raw), defaultURL) {
		t.Fatalf("the config file has no controller URL to redirect:\n%s", raw)
	}
	if err := os.WriteFile(e.config, []byte(strings.ReplaceAll(string(raw), defaultURL, srv.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Exports are named relative to a scratch directory so that output
	// naming the file does not depend on where that directory is.
	t.Chdir(t.TempDir())
	return e
}

// golden compares with a golden file in the package's testdata, wherever
// the test's working directory currently is.
func (e *auditLogEnv) golden(name, got string) {
	e.t.Helper()
	scratch, err := os.Getwd()
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Chdir(e.here)
	testutil.Golden(e.t, name, got)
	e.t.Chdir(scratch)
}

func (e *auditLogEnv) ok(args ...string) string {
	e.t.Helper()
	out, errOut, code := e.run(append([]string{"audit"}, args...)...)
	if code != 0 {
		e.t.Fatalf("taut audit %v exited %d:\n%s\n%s", args, code, out, errOut)
	}
	return out
}

func sha256File(t *testing.T, path string) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), string(raw)
}

func TestAuditLogPlainTable(t *testing.T) {
	e := newAuditLogEnv(t)
	total := len(auditlogtest.Sample())

	errorsOnly := e.ok("--errors")
	e.golden("audit_errors_plain", errorsOnly)
	// Hostile text in a detail never reaches the terminal.
	for _, bad := range []string{"\x1b", "\x07", "\r", "[31m", "pwned"} {
		if strings.Contains(errorsOnly, bad) {
			t.Errorf("plain output contains %q", bad)
		}
	}
	if !strings.Contains(errorsOnly, "Insufficient privileges") {
		t.Error("sanitizing removed the detail text itself")
	}

	e.golden("audit_limit_plain", e.ok("--limit", "5", "--stream", "load:C024BE91L"))
	e.golden("audit_empty_plain", e.ok("--op", "no.such.op"))

	// The summary only claims what is known.
	e.fake.MaxPage = 64
	def := e.ok()
	if !strings.Contains(def, "200 entries") || !strings.Contains(def, "more available") || strings.Contains(def, strconv.Itoa(total)+" entries") {
		t.Errorf("default listing should show 200 entries and say more are available, without a total:\n%s", lastLines(def, 3))
	}
	if n := strings.Count(def, "\n  2026-11-14T"); n != 200 {
		t.Errorf("default listing has %d rows, want 200", n)
	}
	all := e.ok("--limit", "0")
	if !strings.Contains(all, strconv.Itoa(total)+" entries") || !strings.Contains(all, "all that match") || strings.Contains(all, "more available") {
		t.Errorf("--limit 0 should list all %d entries:\n%s", total, lastLines(all, 3))
	}
	exact := e.ok("--errors", "--limit", "9")
	if strings.Contains(exact, "more available") {
		t.Errorf("a limit equal to the number of matches is not a truncated listing:\n%s", lastLines(exact, 3))
	}
	for _, l := range strings.Split(all, "\n") {
		if len([]rune(l)) > 80 {
			t.Fatalf("line wider than 80 columns: %q", l)
		}
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(len(lines)-n, 0):], "\n")
}

func TestAuditLogNDJSON(t *testing.T) {
	e := newAuditLogEnv(t)
	e.golden("audit_ndjson", e.ok("--json", "--stream", "files:C024BE91L"))

	// Every entry exactly once across pages, in order.
	e.fake.MaxPage = 17
	out := e.ok("--json", "--limit", "0")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(auditlogtest.Sample()) {
		t.Fatalf("%d lines, want %d", len(lines), len(auditlogtest.Sample()))
	}
	seen := map[store.LogCursor]bool{}
	var prev store.LogEntry
	for i, l := range lines {
		var entry store.LogEntry
		if err := json.Unmarshal([]byte(l), &entry); err != nil {
			t.Fatalf("line %d is not a JSON object: %v\n%s", i+1, err, l)
		}
		k := store.LogCursor{Stream: entry.Stream, Seq: entry.Seq}
		if seen[k] {
			t.Fatalf("entry %v printed twice", k)
		}
		seen[k] = true
		if i > 0 && (entry.Stream < prev.Stream || (entry.Stream == prev.Stream && entry.Seq <= prev.Seq)) {
			t.Fatalf("line %d out of order", i+1)
		}
		prev = entry
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Error("raw escape byte in NDJSON output")
	}

	if got := strings.Count(e.ok("--json"), "\n"); got != 200 {
		t.Errorf("--json printed %d entries by default, want the limit of 200", got)
	}
	if got := strings.Count(e.ok("--json", "--limit", "7", "--outcome", "ok"), "\n"); got != 7 {
		t.Errorf("--json --limit 7 printed %d entries", got)
	}
	if out := e.ok("--json", "--op", "no.such.op"); out != "" {
		t.Errorf("no matches should print nothing in NDJSON, got %q", out)
	}
}

func TestAuditLogExport(t *testing.T) {
	e := newAuditLogEnv(t)
	e.fake.MaxPage = 4

	out := e.ok("--channel", "general", "--errors", "--export", "general-errors.csv")
	e.golden("audit_export_csv_plain", out)
	sum, body := sha256File(t, "general-errors.csv")
	e.golden("audit_export_csv", body)
	if !strings.Contains(out, sum) {
		t.Errorf("the printed SHA-256 is not the file's (%s):\n%s", sum, out)
	}
	if info, _ := os.Stat("general-errors.csv"); info.Mode().Perm() != 0o600 {
		t.Errorf("export mode = %o, want 600", info.Mode().Perm())
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows[1:] {
		if r[8] != "error" || !strings.HasSuffix(r[0], ":C024BE91L") {
			t.Errorf("row outside the filter: %v", r)
		}
	}

	// An export is complete: the display limit does not apply unless given.
	total := len(auditlogtest.Sample())
	e.ok("--export", "all.ndjson")
	if _, all := sha256File(t, "all.ndjson"); strings.Count(all, "\n") != total {
		t.Errorf("export has %d entries, want all %d", strings.Count(all, "\n"), total)
	}
	out = e.ok("--export", "three.jsonl", "--limit", "3")
	if _, three := sha256File(t, "three.jsonl"); strings.Count(three, "\n") != 3 || !strings.Contains(out, "stopped at --limit") {
		t.Errorf("limited export: %d entries\n%s", strings.Count(three, "\n"), out)
	}

	// --format wins over the extension; formulas are defused in CSV.
	e.ok("--export", "all.txt", "--format", "csv")
	_, text := sha256File(t, "all.txt")
	if !strings.HasPrefix(text, "stream,seq,at,") || !strings.Contains(text, `"'=HYPERLINK(""http://evil.example"")"`) {
		t.Errorf("--format csv did not write guarded CSV:\n%s", lastLines(text, 4))
	}

	// JSON mode reports the export as one document.
	var doc struct {
		Run    string `json:"run"`
		Export struct {
			Path    string `json:"path"`
			Format  string `json:"format"`
			Entries int    `json:"entries"`
			SHA256  string `json:"sha256"`
		} `json:"export"`
	}
	if err := json.Unmarshal([]byte(e.ok("--json", "--export", "doc.csv", "--errors")), &doc); err != nil {
		t.Fatal(err)
	}
	sum, _ = sha256File(t, "doc.csv")
	if doc.Run != auditLogRunID || doc.Export.SHA256 != sum || doc.Export.Entries != 9 || doc.Export.Format != "csv" || doc.Export.Path != "doc.csv" {
		t.Errorf("export document = %+v", doc)
	}
}

func TestAuditLogExportRefusesToOverwrite(t *testing.T) {
	e := newAuditLogEnv(t)
	if err := os.WriteFile("kept.ndjson", []byte("earlier evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run("audit", "--export", "kept.ndjson")
	if code != fail.ExitUsage || out != "" {
		t.Fatalf("exit %d, stdout %q; want exit 2 and nothing on stdout\n%s", code, out, errOut)
	}
	e.golden("audit_export_exists", errOut)
	if _, body := sha256File(t, "kept.ndjson"); body != "earlier evidence\n" {
		t.Fatalf("the existing export was changed: %q", body)
	}
	left, _ := filepath.Glob(".*.tmp")
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	e.ok("--export", "kept.ndjson", "--yes", "--errors")
	if _, body := sha256File(t, "kept.ndjson"); strings.Contains(body, "earlier") || strings.Count(body, "\n") != 9 {
		t.Fatalf("--yes did not replace the export:\n%s", body)
	}
}

func TestAuditLogVerify(t *testing.T) {
	e := newAuditLogEnv(t)
	e.golden("audit_verify_ok", e.ok("--verify"))
	e.golden("audit_verify_export", e.ok("--verify", "--export", "verified.csv", "--stream", "members:0"))

	var doc struct {
		Run      string `json:"run"`
		Verified bool   `json:"verified"`
	}
	if err := json.Unmarshal([]byte(e.ok("--verify", "--json")), &doc); err != nil || !doc.Verified || doc.Run != auditLogRunID {
		t.Errorf("verify document = %+v (%v)", doc, err)
	}

	e.fake.Problem = "stream load:C024BE91L: entry 18 does not match its hash\x1b[2J"
	out, errOut, code := e.run("audit", "--verify")
	if code != fail.ExitFailure || out != "" {
		t.Fatalf("a corrupt audit log exited %d with stdout %q", code, out)
	}
	e.golden("audit_verify_failed", errOut)
	if strings.ContainsRune(errOut, '\x1b') {
		t.Error("the problem text was not sanitized")
	}

	// A corrupt audit log is never exported as verified.
	out, errOut, code = e.run("audit", "--verify", "--export", "corrupt.csv")
	if code != fail.ExitFailure || strings.Contains(out+errOut, "Exported") {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat("corrupt.csv"); err == nil {
		t.Fatal("an export was written although verification failed")
	}
	if !strings.Contains(errOut, "Nothing was exported") {
		t.Errorf("the error does not say the export was withheld:\n%s", errOut)
	}

	out, _, code = e.run("audit", "--verify", "--json")
	var failed struct {
		Error fail.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &failed); err != nil || code != fail.ExitFailure || failed.Error.Code != "audit.corrupt" || failed.Error.Next == "" {
		t.Errorf("JSON failure = %+v (exit %d, %v)", failed, code, err)
	}
}

func TestAuditLogRejectsBadFlags(t *testing.T) {
	e := newAuditLogEnv(t)
	for _, args := range [][]string{
		{"--outcome", "maybe"},
		{"--errors", "--outcome", "ok"},
		{"--since", "yesterday"},
		{"--until", "2h30"},
		{"--since", "-2h"},
		{"--since", "1h", "--until", "2h"},
		{"--since", "2026-11-14T10:00:00Z", "--until", "2026-11-14T10:00:00Z"},
		{"--limit", "-1"},
		{"--limit", "many"},
		{"--format", "csv"},
		{"--export", "x.out", "--format", "xml"},
		{"--channel", "general", "--stream", "load:C024BE91L"},
		{"--channel", "no-such-channel"},
		{"--export", "."},
		{"extra"},
	} {
		out, errOut, code := e.run(append([]string{"audit"}, args...)...)
		if code != fail.ExitUsage {
			t.Errorf("%v: exit %d, want 2\n%s%s", args, code, out, errOut)
		}
		if out != "" || !strings.Contains(errOut, "✗") {
			t.Errorf("%v: want an error block on stderr only, got stdout %q stderr %q", args, out, errOut)
		}
	}
	if n := len(e.fake.Calls()); n != 0 {
		t.Errorf("the audit log was read %d times for invalid command lines", n)
	}
	// Still structured under --json.
	out, _, code := e.run("audit", "--outcome", "maybe", "--json")
	var doc struct {
		Error fail.Error `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != fail.ExitUsage || doc.Error.Next == "" {
		t.Errorf("JSON usage error = %q (exit %d, %v)", out, code, err)
	}
}

func TestAuditLogTimeFilters(t *testing.T) {
	e := newAuditLogEnv(t)
	last := func(args ...string) store.LogFilter {
		t.Helper()
		e.ok(append(args, "--json", "--limit", "1")...)
		calls := e.fake.Calls()
		return calls[0]
	}
	within := func(got, want time.Time) bool {
		d := got.Sub(want)
		return d > -time.Minute && d < time.Minute
	}
	now := time.Now()
	for _, tc := range []struct {
		value string
		want  time.Time
	}{
		{"2h", now.Add(-2 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"3d", now.Add(-72 * time.Hour)},
		{"1w2d", now.Add(-9 * 24 * time.Hour)},
		{"1.5h", now.Add(-90 * time.Minute)},
		{"1h30m", now.Add(-90 * time.Minute)},
		{"45s", now.Add(-45 * time.Second)},
	} {
		e.fake = auditlogtest.New(auditlogtest.Sample())
		if got := last("--since", tc.value).Since; !within(got, tc.want) {
			t.Errorf("--since %s = %v, want about %v", tc.value, got, tc.want)
		}
	}
	for _, tc := range []struct {
		value string
		want  time.Time
	}{
		{"2026-11-14T09:02:00Z", time.Date(2026, 11, 14, 9, 2, 0, 0, time.UTC)},
		{"2026-11-14T09:02:00.5+02:00", time.Date(2026, 11, 14, 7, 2, 0, 5e8, time.UTC)},
		{"2026-11-14", time.Date(2026, 11, 14, 0, 0, 0, 0, time.Local)},
		{"2026-11-14 09:02", time.Date(2026, 11, 14, 9, 2, 0, 0, time.Local)},
		{"2026-11-14T09:02:03", time.Date(2026, 11, 14, 9, 2, 3, 0, time.Local)},
	} {
		e.fake = auditlogtest.New(auditlogtest.Sample())
		if got := last("--until", tc.value).Until; !got.Equal(tc.want) {
			t.Errorf("--until %s = %v, want %v", tc.value, got, tc.want)
		}
	}

	// And the window is applied: at or after since, before until.
	e.fake = auditlogtest.New(auditlogtest.Sample())
	out := e.ok("--json", "--limit", "0", "--since", "2026-11-14T09:01:00Z", "--until", "2026-11-14T09:02:00Z")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 40 {
		t.Errorf("a one-minute window of entries 1.5s apart has %d entries, want 40", len(lines))
	}
	for _, l := range lines {
		var entry store.LogEntry
		_ = json.Unmarshal([]byte(l), &entry)
		if entry.At.Before(time.Date(2026, 11, 14, 9, 1, 0, 0, time.UTC)) || !entry.At.Before(time.Date(2026, 11, 14, 9, 2, 0, 0, time.UTC)) {
			t.Fatalf("entry outside the window: %v", entry.At)
		}
	}
}

func TestAuditLogChannelAndFilters(t *testing.T) {
	e := newAuditLogEnv(t)
	streamsOf := func(args ...string) []string {
		t.Helper()
		e.fake = auditlogtest.New(auditlogtest.Sample())
		e.ok(append(args, "--json", "--limit", "0")...)
		var streams []string
		for _, c := range e.fake.Calls() {
			if len(streams) == 0 || streams[len(streams)-1] != c.Stream {
				streams = append(streams, c.Stream)
			}
		}
		return streams
	}
	for _, channel := range []string{"general", "#general", "C024BE91L", "c024be91l"} {
		got := strings.Join(streamsOf("--channel", channel), " ")
		if got != "files:C024BE91L load:C024BE91L finalize:C024BE91L" {
			t.Errorf("--channel %s read streams %q", channel, got)
		}
	}
	// A direct message is named by its Slack ID, and by nothing else: not
	// by who is in it, and not by the name Slack generated from them.
	for _, id := range []string{"D024BE91L", "G07QX3M2K"} {
		if got := strings.Join(streamsOf("--channel", id), " "); got != "load:"+id+" finalize:"+id {
			t.Errorf("--channel %s read streams %q", id, got)
		}
	}
	for _, name := range []string{"Ada Abbott, Ben Banerjee", "mpdm-ada--ben--cy-1"} {
		out, errOut, code := e.run("audit", "--channel", name, "--json")
		if flat := strings.Join(strings.Fields(out+errOut), " "); code != fail.ExitUsage || !strings.Contains(flat, "no channel named") {
			t.Errorf("--channel %q: exit %d, want a usage error:\n%s\n%s", name, code, out, errOut)
		}
	}
	// An ID the run does not list is still accepted as an ID.
	if got := strings.Join(streamsOf("--channel", "C0UNKNOWN1"), " "); got != "files:C0UNKNOWN1 load:C0UNKNOWN1 finalize:C0UNKNOWN1" {
		t.Errorf("unlisted channel ID read streams %q", got)
	}

	e.fake = auditlogtest.New(auditlogtest.Sample())
	out := e.ok("--channel", "random", "--json", "--limit", "0")
	n := 0
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var entry store.LogEntry
		_ = json.Unmarshal([]byte(l), &entry)
		if !strings.HasSuffix(entry.Stream, ":C07QX3M2K") {
			t.Fatalf("--channel random printed %s", entry.Stream)
		}
		n++
	}
	want := 0
	for _, s := range auditlogtest.Sample() {
		if strings.HasSuffix(s.Stream, ":C07QX3M2K") {
			want++
		}
	}
	if n != want {
		t.Errorf("--channel random printed %d entries, want %d", n, want)
	}

	// The remaining filters reach the control plane as given.
	e.fake = auditlogtest.New(auditlogtest.Sample())
	e.ok("--stream", "load:C024BE91L", "--op", "message.fail", "--outcome", "error", "--grep", "Forbidden", "--json")
	c := e.fake.Calls()[0]
	if c.Stream != "load:C024BE91L" || c.Op != "message.fail" || c.Outcome != "error" || c.Contains != "Forbidden" {
		t.Errorf("filter sent = %+v", c)
	}
	e.ok("--errors", "--json")
	if c := e.fake.Calls(); c[len(c)-1].Outcome != "error" {
		t.Error("--errors did not filter by outcome")
	}
}

func TestAuditLogRunSelection(t *testing.T) {
	e := newAuditLogEnv(t)
	if out := e.ok("--run", auditLogRunID, "--verify"); !strings.Contains(out, auditLogRunID) {
		t.Errorf("--run output does not name the run:\n%s", out)
	}
	_, errOut, code := e.run("audit", "--run", "nope")
	if code == 0 || !strings.Contains(errOut, "No such run") {
		t.Errorf("unknown --run: exit %d\n%s", code, errOut)
	}
	e.noRun = true
	_, errOut, code = e.run("audit")
	if code == 0 || !strings.Contains(errOut, "No migration has been started for fixture") || !strings.Contains(errOut, "taut migrate") {
		t.Errorf("no run: exit %d\n%s", code, errOut)
	}
}

func TestAuditLogSourceFailureIsActionable(t *testing.T) {
	e := newAuditLogEnv(t)
	e.fake.Fail(fail.New("store.unavailable", "The ledger database is not reachable", "connection refused", "Check Postgres, then retry."))
	for _, args := range [][]string{{}, {"--json"}, {"--export", "x.csv"}, {"--verify"}} {
		out, errOut, code := e.run(append([]string{"audit"}, args...)...)
		if code != fail.ExitFailure {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
		if len(args) > 0 && args[0] == "--json" {
			if !strings.Contains(out, `"next"`) {
				t.Errorf("--json failure has no next step: %s", out)
			}
			continue
		}
		if !regexp.MustCompile(`(?s)✗ .+\n.+└ `).MatchString(errOut) {
			t.Errorf("%v: failure is not a what/why/next block:\n%s", args, errOut)
		}
	}
	if _, err := os.Stat("x.csv"); err == nil {
		t.Error("a failed export left a file")
	}
}

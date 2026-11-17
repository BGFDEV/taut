package auditlog

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/auditlog/auditlogtest"
)

func collect(t *testing.T, src Source, f store.LogFilter, limit int) ([]store.LogEntry, bool) {
	t.Helper()
	var got []store.LogEntry
	more, err := Walk(context.Background(), src, f, limit, func(e store.LogEntry) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, more
}

func keyOf(e store.LogEntry) store.LogCursor { return store.LogCursor{Stream: e.Stream, Seq: e.Seq} }

// Every entry must come back exactly once, in order, whatever the page size,
// and paging must stop when the source reports no further page.
func TestWalkVisitsEveryEntryOnce(t *testing.T) {
	sample := auditlogtest.Sample()
	for _, tc := range []struct {
		name  string
		page  int
		eager bool
	}{{"one page", 0, false}, {"pages of 7", 7, false}, {"pages of 1", 1, false}, {"eager cursor", 7, true}, {"eager, exact multiple", 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			src := auditlogtest.New(sample)
			src.MaxPage, src.EagerNext = tc.page, tc.eager
			got, more := collect(t, src, store.LogFilter{}, 0)
			if more {
				t.Error("more reported after reading everything")
			}
			if len(got) != len(sample) {
				t.Fatalf("got %d entries, want %d", len(got), len(sample))
			}
			seen := map[store.LogCursor]bool{}
			for i, e := range got {
				if seen[keyOf(e)] {
					t.Fatalf("entry %v returned twice", keyOf(e))
				}
				seen[keyOf(e)] = true
				if i > 0 {
					p := got[i-1]
					if e.Stream < p.Stream || (e.Stream == p.Stream && e.Seq <= p.Seq) {
						t.Fatalf("out of order: %v after %v", keyOf(e), keyOf(p))
					}
				}
			}
			calls := src.Calls()
			if last := calls[len(calls)-1]; tc.page > 0 && last.After == (store.LogCursor{}) {
				t.Error("the last call did not resume from a cursor")
			}
		})
	}
}

func TestWalkLimitAndMore(t *testing.T) {
	sample := auditlogtest.New(auditlogtest.Sample())
	sample.MaxPage = 9
	errs, _ := collect(t, sample, store.LogFilter{Outcome: "error"}, 0)
	if len(errs) < 3 {
		t.Fatalf("sample has %d errors; the test needs at least 3", len(errs))
	}
	for _, tc := range []struct {
		limit    int
		want     int
		wantMore bool
	}{
		{1, 1, true},
		{len(errs) - 1, len(errs) - 1, true},
		{len(errs), len(errs), false}, // exactly the total: nothing more
		{len(errs) + 5, len(errs), false},
		{0, len(errs), false},
	} {
		got, more := collect(t, sample, store.LogFilter{Outcome: "error"}, tc.limit)
		if len(got) != tc.want || more != tc.wantMore {
			t.Errorf("limit %d: got %d entries, more=%v; want %d, more=%v", tc.limit, len(got), more, tc.want, tc.wantMore)
		}
		for _, e := range got {
			if e.Outcome != "error" {
				t.Errorf("limit %d: filter leaked %v", tc.limit, keyOf(e))
			}
		}
	}
}

// stuck returns the same cursor forever.
type stuck struct{}

func (stuck) AuditLog(context.Context, store.LogFilter) (controller.LogPage, error) {
	return controller.LogPage{
		Entries: []store.LogEntry{{Stream: "load:C1", Seq: 1}},
		Next:    &store.LogCursor{Stream: "load:C1", Seq: 1},
	}, nil
}
func (stuck) Verify(context.Context) (bool, string, error) { return true, "", nil }

func TestWalkStopsOnStalledCursor(t *testing.T) {
	n := 0
	_, err := Walk(context.Background(), stuck{}, store.LogFilter{}, 0, func(store.LogEntry) error { n++; return nil })
	if fe := fail.From(err); err == nil || fe.Code != "audit.cursor_stalled" {
		t.Fatalf("err = %v, want audit.cursor_stalled", err)
	}
	if n > 2 {
		t.Fatalf("visited %d entries before noticing the stall", n)
	}
}

func TestWalkPropagatesErrors(t *testing.T) {
	src := auditlogtest.New(auditlogtest.Sample())
	boom := errors.New("boom")
	if _, err := Walk(context.Background(), src, store.LogFilter{}, 0, func(store.LogEntry) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("callback error lost: %v", err)
	}
	src.Fail(boom)
	if _, err := Walk(context.Background(), src, store.LogFilter{}, 0, func(store.LogEntry) error { return nil }); !errors.Is(err, boom) {
		t.Fatalf("source error lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Walk(ctx, auditlogtest.New(nil), store.LogFilter{}, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context ignored: %v", err)
	}
}

func TestStreamsReadsEachStreamInTurn(t *testing.T) {
	sample := auditlogtest.Sample()
	streams := []string{"files:C024BE91L", "load:C024BE91L", "finalize:C024BE91L", "finalize:CMISSING"}
	var want []store.LogCursor
	for _, s := range streams {
		for _, e := range sample {
			if e.Stream == s {
				want = append(want, keyOf(e))
			}
		}
	}
	for _, page := range []int{0, 1, 4, 50} {
		for _, eager := range []bool{false, true} {
			base := auditlogtest.New(sample)
			base.MaxPage, base.EagerNext = page, eager
			got, more := collect(t, Streams(base, streams), store.LogFilter{Stream: "ignored"}, 0)
			if more || len(got) != len(want) {
				t.Fatalf("page %d eager %v: got %d entries (more=%v), want %d", page, eager, len(got), more, len(want))
			}
			for i, e := range got {
				if keyOf(e) != want[i] {
					t.Fatalf("page %d eager %v: entry %d is %v, want %v", page, eager, i, keyOf(e), want[i])
				}
			}
		}
	}

	// Other filters still apply within each stream.
	base := auditlogtest.New(sample)
	base.MaxPage = 3
	got, _ := collect(t, Streams(base, streams), store.LogFilter{Outcome: "error"}, 0)
	if len(got) == 0 {
		t.Fatal("no errors found across the channel's streams")
	}
	for _, e := range got {
		if e.Outcome != "error" || !strings.HasSuffix(e.Stream, ":C024BE91L") {
			t.Fatalf("unexpected entry %v (%s)", keyOf(e), e.Outcome)
		}
	}

	if _, err := Streams(base, streams).AuditLog(context.Background(), store.LogFilter{After: store.LogCursor{Stream: "team:0", Seq: 1}}); err == nil {
		t.Fatal("a cursor outside the streams was accepted")
	}
}

func TestSanitize(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "graph: 403 Forbidden", "graph: 403 Forbidden"},
		{"empty", "", ""},
		{"sgr", "\x1b[31mred\x1b[0m alert", "red alert"},
		{"cursor and clear", "a\x1b[2J\x1b[Hb", "ab"},
		{"osc title", "x\x1b]0;pwned\x07y", "xy"},
		{"osc 8 link", "\x1b]8;;http://evil.example\x1b\\click\x1b]8;;\x1b\\", "click"},
		{"c1 csi", "a\u009b31mb", "a31mb"},
		{"bare controls", "a\x00b\x07c\x08d\x7fe", "abcde"},
		{"line breaks", "one\r\ntwo\n\tthree", "one two three"},
		{"bidi override", "user\u202Etxt.exe", "usertxt.exe"},
		{"zero width", "ad\u200Bmin\uFEFF", "admin"},
		{"invalid utf8", "a\xffb", "ab"},
		{"unicode kept", "Zoë → 東京 ✓", "Zoë → 東京 ✓"},
		{"spaces collapsed", "  a   b  ", "a b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Sanitize(tc.in)
			if got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Whatever goes in, nothing that can drive a terminal comes out.
func TestSanitizeNeverEmitsControls(t *testing.T) {
	inputs := []string{
		"\x1b", "\x1b[", "\x1b]0;never terminated", "\x1bP1$r\x1b\\", "\x9b1;1H", "\x1b[38;2;1;2;3m",
		"tail\x1b", "\x1b\x1b\x1b[[[", "a\u2028b\u2029c", "\x1b_apc\x1b\\rest", "\x1b^pm\x07x",
	}
	for _, e := range auditlogtest.Sample() {
		inputs = append(inputs, e.Detail, e.SourceID)
	}
	for _, in := range inputs {
		for _, r := range Sanitize(in) {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
				t.Errorf("Sanitize(%q) kept %U", in, r)
			}
		}
	}
}

func TestCell(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"plain":            "plain",
		"=1+1":             "'=1+1",
		"+1":               "'+1",
		"-1":               "'-1",
		"@SUM(A1)":         "'@SUM(A1)",
		"\t=1":             "'\t=1",
		"\r=1":             "'\r=1",
		"a=b":              "a=b",
		"C024BE91L/17.001": "C024BE91L/17.001",
		"'already":         "'already",
	} {
		if got := Cell(in); got != want {
			t.Errorf("Cell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormats(t *testing.T) {
	for path, want := range map[string]Format{
		"a.csv": CSV, "A.CSV": CSV, "a.ndjson": NDJSON, "a.jsonl": NDJSON, "a": NDJSON, "dir.csv/a.txt": NDJSON,
	} {
		if got := FormatFor(path); got != want {
			t.Errorf("FormatFor(%q) = %q, want %q", path, got, want)
		}
	}
	if f, ok := ParseFormat(" CSV "); !ok || f != CSV {
		t.Errorf("ParseFormat(CSV) = %q, %v", f, ok)
	}
	if _, ok := ParseFormat("xml"); ok {
		t.Error("ParseFormat accepted xml")
	}
}

func fileSHA(t *testing.T, path string) (string, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), raw
}

func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if strings.HasSuffix(n.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", n.Name())
		}
	}
}

func TestExportNDJSON(t *testing.T) {
	dir := t.TempDir()
	sample := auditlogtest.Sample()
	src := auditlogtest.New(sample)
	src.MaxPage = 25
	path := filepath.Join(dir, "out.ndjson")
	res, err := Export(context.Background(), src, store.LogFilter{}, ExportOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	sum, raw := fileSHA(t, path)
	if res.SHA256 != sum || res.Bytes != int64(len(raw)) || res.Entries != len(sample) || res.Format != NDJSON || res.Truncated {
		t.Errorf("result %+v does not describe the file (sha %s, %d bytes, %d entries)", res, sum, len(raw), len(sample))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	noTempFiles(t, dir)

	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != len(sample) {
		t.Fatalf("%d lines, want %d", len(lines), len(sample))
	}
	want := collectAll(t, auditlogtest.New(sample))
	for i, l := range lines {
		var e store.LogEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %d is not JSON: %v", i+1, err)
		}
		if e != want[i] {
			t.Fatalf("line %d round-trips to %+v, want %+v", i+1, e, want[i])
		}
	}
	// The export holds entries as recorded: escape sequences survive, but
	// only as JSON escapes, never as raw bytes.
	if strings.ContainsRune(string(raw), '\x1b') {
		t.Error("raw ESC byte in NDJSON")
	}
	if !strings.Contains(string(raw), `\u001b[31m`) {
		t.Error("the recorded detail was altered in the export")
	}
}

func collectAll(t *testing.T, src Source) []store.LogEntry {
	got, _ := collect(t, src, store.LogFilter{}, 0)
	return got
}

func TestExportCSV(t *testing.T) {
	dir := t.TempDir()
	sample := auditlogtest.Sample()
	path := filepath.Join(dir, "out.csv")
	res, err := Export(context.Background(), auditlogtest.New(sample), store.LogFilter{}, ExportOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != CSV {
		t.Fatalf("format = %q, want csv", res.Format)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("not valid CSV: %v", err)
	}
	if len(rows) != len(sample)+1 {
		t.Fatalf("%d rows, want %d and a header", len(rows), len(sample))
	}
	if strings.Join(rows[0], ",") != "stream,seq,at,actor,op,sourceId,targetId,contentHash,outcome,detail,prevHash,entryHash" {
		t.Errorf("header = %v", rows[0])
	}
	want := collectAll(t, auditlogtest.New(sample))
	formulas := 0
	for i, r := range rows[1:] {
		e := want[i]
		if r[0] != e.Stream || r[4] != e.Op || r[8] != e.Outcome || r[11] != e.EntryHash {
			t.Fatalf("row %d = %v, want entry %v", i+1, r, keyOf(e))
		}
		if r[2] != e.At.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") || !strings.HasSuffix(r[2], "Z") {
			t.Fatalf("row %d time = %q", i+1, r[2])
		}
		for j, c := range r {
			if c != "" && strings.ContainsRune("=+-@\t\r", rune(c[0])) {
				t.Errorf("row %d column %s could be run as a formula: %q", i+1, rows[0][j], c)
			}
		}
		if strings.HasPrefix(r[5], "'=") {
			formulas++
		}
	}
	if formulas != 1 {
		t.Errorf("%d guarded formula cells, want the 1 in the sample", formulas)
	}
}

func TestExportRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.ndjson")
	if err := os.WriteFile(path, []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := auditlogtest.New(auditlogtest.Sample())
	_, err := Export(context.Background(), src, store.LogFilter{}, ExportOptions{Path: path})
	fe := fail.From(err)
	if err == nil || fe.Code != CodeExists || fe.Next == "" {
		t.Fatalf("err = %v, want %s with a next step", err, CodeExists)
	}
	if len(src.Calls()) != 0 {
		t.Error("the audit log was read before the refusal")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "precious\n" {
		t.Fatalf("existing file was changed: %q", raw)
	}
	noTempFiles(t, dir)

	res, err := Export(context.Background(), src, store.LogFilter{Outcome: "error"}, ExportOptions{Path: path, Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	sum, raw := fileSHA(t, path)
	if sum != res.SHA256 || strings.Contains(string(raw), "precious") {
		t.Fatal("overwrite did not replace the file")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("replaced file mode = %o, want 600", info.Mode().Perm())
	}
	noTempFiles(t, dir)
}

// appearing creates the target file while the export is being read, as
// another process might.
type appearing struct {
	Source
	path string
}

func (a appearing) AuditLog(ctx context.Context, f store.LogFilter) (controller.LogPage, error) {
	_ = os.WriteFile(a.path, []byte("theirs\n"), 0o600)
	return a.Source.AuditLog(ctx, f)
}

func TestExportDoesNotReplaceAFileThatAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.ndjson")
	src := appearing{Source: auditlogtest.New(auditlogtest.Sample()), path: path}
	_, err := Export(context.Background(), src, store.LogFilter{}, ExportOptions{Path: path})
	if fail.From(err).Code != CodeExists {
		t.Fatalf("err = %v, want %s", err, CodeExists)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "theirs\n" {
		t.Fatalf("the other file was replaced: %q", raw)
	}
	noTempFiles(t, dir)
}

func TestExportLeavesNothingOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	src := auditlogtest.New(auditlogtest.Sample())
	src.Fail(fail.New("controller.unreachable", "Could not reach the controller", "connection refused", "Start it."))
	_, err := Export(context.Background(), src, store.LogFilter{}, ExportOptions{Path: path})
	if fail.From(err).Code != "controller.unreachable" {
		t.Fatalf("err = %v, want the source's error", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a partial export was left under the final name")
	}
	noTempFiles(t, dir)

	// Unwritable or wrong destinations are structured errors too.
	for _, bad := range []string{filepath.Join(dir, "missing", "out.csv"), dir, " "} {
		src.Fail(nil)
		_, err := Export(context.Background(), src, store.LogFilter{}, ExportOptions{Path: bad})
		if fe := fail.From(err); err == nil || fe.Code == "unexpected" || fe.Next == "" {
			t.Errorf("Export(%q): err = %v, want a structured error with a next step", bad, err)
		}
	}
	noTempFiles(t, dir)
}

func TestExportLimitAndEmpty(t *testing.T) {
	dir := t.TempDir()
	src := auditlogtest.New(auditlogtest.Sample())
	res, err := Export(context.Background(), src, store.LogFilter{}, ExportOptions{Path: filepath.Join(dir, "some.csv"), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 10 || !res.Truncated {
		t.Errorf("limited export: %+v", res)
	}
	res, err = Export(context.Background(), src, store.LogFilter{Op: "no.such.op"}, ExportOptions{Path: filepath.Join(dir, "none.csv")})
	if err != nil {
		t.Fatal(err)
	}
	_, raw := fileSHA(t, res.Path)
	if res.Entries != 0 || res.Truncated || strings.Count(string(raw), "\n") != 1 {
		t.Errorf("empty export: %+v, file %q", res, raw)
	}
}

func TestComma(t *testing.T) {
	for n, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 12431: "12,431", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := Comma(n); got != want {
			t.Errorf("Comma(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestExportGoldens(t *testing.T) {
	dir := t.TempDir()
	src := auditlogtest.New(auditlogtest.Sample())
	f := store.LogFilter{Stream: "files:C024BE91L"}
	for _, name := range []string{"export.csv", "export.ndjson"} {
		res, err := Export(context.Background(), src, f, ExportOptions{Path: filepath.Join(dir, name)})
		if err != nil {
			t.Fatal(err)
		}
		_, raw := fileSHA(t, res.Path)
		testutil.Golden(t, strings.ReplaceAll(name, ".", "_"), string(raw))
	}
}

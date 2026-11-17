package auditlog

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/auditlog/auditlogtest"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
)

func init() { spinEvery = time.Microsecond }

// harness drives the screen inside the real shell, running the commands it
// returns by hand so tests decide when a page "arrives".
type harness struct {
	t   *testing.T
	m   tea.Model
	scr *Screen
	src *auditlogtest.Fake
	// pending are commands returned but not yet run.
	pending []tea.Cmd
}

func newHarness(t *testing.T, th theme.Theme, w, h int, o Options) *harness {
	t.Helper()
	src := auditlogtest.New(auditlogtest.Sample())
	src.MaxPage = 30
	scr := NewScreen(src, o)
	x := &harness{t: t, scr: scr, src: src}
	x.m = shell.New(scr, shell.Options{Theme: th, Command: "audit", Context: "acme-migration", LockTheme: true})
	x.send(tea.WindowSizeMsg{Width: w, Height: h})
	x.pending = append(x.pending, scr.Init())
	return x
}

// open is a harness with everything loaded that wants to load.
func open(t *testing.T, th theme.Theme, w, h int) *harness {
	return newHarness(t, th, w, h, Options{}).settle()
}

func (x *harness) send(msg tea.Msg) *harness {
	var cmd tea.Cmd
	x.m, cmd = x.m.Update(msg)
	if cmd != nil {
		x.pending = append(x.pending, cmd)
	}
	return x
}

// settle runs pending commands, and the commands they lead to, until the
// screen is idle. Spinner ticks are dropped so frames stay deterministic.
func (x *harness) settle() *harness {
	for guard := 0; len(x.pending) > 0; guard++ {
		if guard > 10000 {
			x.t.Fatal("the screen never became idle")
		}
		cmd := x.pending[0]
		x.pending = x.pending[1:]
		if cmd == nil {
			continue
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			x.pending = append(x.pending, msg...)
		case tickMsg, nil:
		default:
			if strings.Contains(strings.ToLower(typeName(msg)), "blink") {
				continue
			}
			x.send(msg)
		}
	}
	return x
}

func typeName(v any) string {
	type named interface{ String() string }
	if n, ok := v.(named); ok {
		return n.String()
	}
	return strings.TrimPrefix(strings.TrimPrefix(typeOf(v), "*"), "cursor.")
}

func (x *harness) press(keys ...string) *harness {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "right":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "pgdown":
			msg = tea.KeyPressMsg{Code: tea.KeyPgDown}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		x.send(msg)
	}
	return x
}

func (x *harness) typ(text string) *harness {
	for _, r := range text {
		x.press(string(r))
	}
	return x
}

func (x *harness) frame() string { return testutil.Plain(x.m.View().Content) }

func TestScreenSnapshots(t *testing.T) {
	dark := theme.Default()
	ascii := theme.New(theme.Options{Dark: true, Unicode: false})
	boom := fail.New("controller.unreachable", "Could not reach the controller", "dial tcp 127.0.0.1:7070: connection refused", "Start it with `taut controller`.")

	cases := []struct {
		name  string
		frame func() string
	}{
		{"initial_80x24", func() string { return open(t, dark, 80, 24).frame() }},
		{"initial_120x32", func() string { return open(t, dark, 120, 32).frame() }},
		{"first_load_80x24", func() string { return newHarness(t, dark, 80, 24, Options{}).frame() }},
		{"detail_80x24", func() string { return open(t, dark, 80, 24).press("down", "down", "enter").frame() }},
		{"detail_error_80x24", func() string {
			return newHarness(t, dark, 80, 24, Options{Filter: store.LogFilter{Outcome: "error", Stream: "load:C024BE91L"}, Scope: "#general"}).
				settle().press("enter").frame()
		}},
		{"filter_editing_80x24", func() string { return open(t, dark, 80, 24).press("/").typ("403").frame() }},
		{"filter_applied_80x24", func() string { return open(t, dark, 80, 24).press("/").typ("403").press("enter").settle().frame() }},
		{"errors_only_80x24", func() string { return open(t, dark, 80, 24).press("o", "o").settle().frame() }},
		{"op_picker_80x24", func() string { return open(t, dark, 80, 24).press("p", "down", "down").frame() }},
		{"op_applied_80x24", func() string {
			return open(t, dark, 80, 24).press("p", "down", "down", "enter").settle().frame()
		}},
		{"loading_more_80x24", func() string { return open(t, dark, 80, 24).press("G").frame() }},
		{"page_error_80x24", func() string {
			x := open(t, dark, 80, 24)
			x.src.Fail(boom)
			return x.press("G").settle().frame()
		}},
		{"empty_80x24", func() string {
			return newHarness(t, dark, 80, 24, Options{Filter: store.LogFilter{Op: "no.such.op"}}).settle().frame()
		}},
		{"verify_ok_80x24", func() string { return open(t, dark, 80, 24).press("v").settle().frame() }},
		{"verify_failed_80x24", func() string {
			x := open(t, dark, 80, 24)
			x.src.Problem = "stream load:C024BE91L: entry 18 does not match its hash"
			return x.press("v").settle().frame()
		}},
		{"export_prompt_80x24", func() string { return open(t, dark, 80, 24).press("e").frame() }},
		{"narrow_60x16", func() string { return open(t, dark, 60, 16).frame() }},
		{"narrow_detail_60x16", func() string { return open(t, dark, 60, 16).press("enter").frame() }},
		{"ascii_80x24", func() string { return open(t, ascii, 80, 24).press("down").frame() }},
		{"help_80x24", func() string { return open(t, dark, 80, 24).press("?").frame() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { testutil.Golden(t, c.name, c.frame()) })
	}
}

func TestScreenExport(t *testing.T) {
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// The path typed into the prompt is relative, so the frames do not
	// depend on where the temporary directory is.
	golden := func(name string, x *harness) {
		t.Helper()
		t.Chdir(here)
		testutil.Golden(t, name, x.frame())
		t.Chdir(dir)
	}
	t.Chdir(dir)
	x := open(t, theme.Default(), 80, 24).press("o", "o").settle() // errors only
	x.press("e")
	for range len("audit.ndjson") {
		x.press("backspace")
	}
	x.typ("errors.csv").press("enter").settle()
	golden("export_done_80x24", x)

	raw, err := os.ReadFile(filepath.Join(dir, "errors.csv"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := collect(t, auditlogtest.New(auditlogtest.Sample()), store.LogFilter{Outcome: "error"}, 0)
	if len(rows) != len(want)+1 {
		t.Fatalf("export has %d rows, want %d errors and a header:\n%s", len(rows), len(want), raw)
	}

	// Exporting to the same name again asks before replacing.
	x.press("esc", "e")
	for range len("audit.ndjson") {
		x.press("backspace")
	}
	x.typ("errors.csv").press("enter").settle()
	golden("export_confirm_80x24", x)
	if !x.scr.CapturingInput() {
		t.Error("q would quit while the replace question is open")
	}
	x.press("n").settle()
	if got, _ := os.ReadFile(filepath.Join(dir, "errors.csv")); string(got) != string(raw) {
		t.Fatal("declining replaced the file")
	}
	if !strings.Contains(x.frame(), "existing file was kept") {
		t.Errorf("no confirmation that the file was kept:\n%s", x.frame())
	}

	// And replaces on yes, with the filter in force at the time.
	x.press("o").settle() // all outcomes
	x.press("e")
	for range len("audit.ndjson") {
		x.press("backspace")
	}
	x.typ("errors.csv").press("enter").settle().press("y").settle()
	got, _ := os.ReadFile(filepath.Join(dir, "errors.csv"))
	all, err := csv.NewReader(strings.NewReader(string(got))).ReadAll()
	if err != nil || len(all) != len(auditlogtest.Sample())+1 {
		t.Fatalf("replacing export has %d rows (%v), want every entry and a header", len(all), err)
	}
}

// Every entry is listed exactly once however the pages arrive, and loading
// stops when the source says there is no more.
func TestScreenLazyPaging(t *testing.T) {
	x := open(t, theme.Default(), 80, 24)
	total := len(auditlogtest.Sample())
	if n := len(x.scr.entries); n == 0 || n >= total {
		t.Fatalf("loaded %d of %d entries up front; paging should be lazy", n, total)
	}
	if x.scr.done {
		t.Fatal("done before everything was loaded")
	}
	for i := 0; i < total && !x.scr.done; i++ {
		x.press("G").settle()
	}
	if !x.scr.done || len(x.scr.entries) != total {
		t.Fatalf("loaded %d of %d entries, done=%v", len(x.scr.entries), total, x.scr.done)
	}
	seen := map[store.LogCursor]bool{}
	for _, e := range x.scr.entries {
		if seen[keyOf(e)] {
			t.Fatalf("entry %v listed twice", keyOf(e))
		}
		seen[keyOf(e)] = true
	}
	calls := len(x.src.Calls())
	x.press("G", "pgdown", "down").settle()
	if len(x.src.Calls()) != calls {
		t.Error("kept asking for pages after the last one")
	}
	if !strings.Contains(x.frame(), Comma(total)+" of "+Comma(total)) {
		t.Errorf("position does not show the known total:\n%s", x.frame())
	}
}

func TestScreenPageErrorKeepsEntriesAndRetries(t *testing.T) {
	x := open(t, theme.Default(), 80, 24)
	loaded := len(x.scr.entries)
	x.src.Fail(errors.New("connection reset"))
	x.press("G").settle()
	if len(x.scr.entries) != loaded {
		t.Fatalf("entries changed on a failed page: %d → %d", loaded, len(x.scr.entries))
	}
	if f := x.frame(); !strings.Contains(f, "retry") || !strings.Contains(f, "message.import") {
		t.Fatalf("error frame lost the entries or the retry hint:\n%s", f)
	}
	// No automatic retry storm while the error stands.
	calls := len(x.src.Calls())
	x.press("down", "up", "G").settle()
	if len(x.src.Calls()) != calls {
		t.Error("fetched again without being asked to retry")
	}
	x.src.Fail(nil)
	x.press("r").settle()
	if len(x.scr.entries) <= loaded || x.scr.loadErr != nil {
		t.Fatalf("retry did not load more: %d entries, err %v", len(x.scr.entries), x.scr.loadErr)
	}
}

// A page for a filter the reader has since changed must not be mixed in.
func TestScreenDropsStalePages(t *testing.T) {
	x := newHarness(t, theme.Default(), 80, 24, Options{})
	first := x.pending // the unfiltered first page, still in flight
	x.pending = nil
	x.press("o", "o") // errors only
	x.pending = append(x.pending, first...)
	x.settle()
	if len(x.scr.entries) == 0 {
		t.Fatal("nothing loaded")
	}
	for _, e := range x.scr.entries {
		if e.Outcome != "error" {
			t.Fatalf("stale page leaked %v (%s) into an errors-only list", keyOf(e), e.Outcome)
		}
	}
}

func TestScreenNeverBlocksOnTheSource(t *testing.T) {
	x := open(t, theme.Default(), 80, 24)
	calls := len(x.src.Calls())
	x.press("G", "o", "v", "/")
	if len(x.src.Calls()) != calls {
		t.Fatal("Update called the source directly instead of returning a command")
	}
}

func TestScreenFramesAreStable(t *testing.T) {
	for _, size := range [][2]int{{60, 14}, {60, 16}, {80, 24}, {97, 31}, {120, 40}, {200, 60}} {
		w, h := size[0], size[1]
		states := map[string]func() *harness{
			"list":    func() *harness { return open(t, theme.Default(), w, h) },
			"loading": func() *harness { return newHarness(t, theme.Default(), w, h, Options{}) },
			"bottom":  func() *harness { return open(t, theme.Default(), w, h).press("G") },
			"detail":  func() *harness { return open(t, theme.Default(), w, h).press("enter") },
			"filter": func() *harness {
				return open(t, theme.Default(), w, h).press("/").typ(strings.Repeat("long filter ", 12))
			},
			"ops":    func() *harness { return open(t, theme.Default(), w, h).press("p") },
			"export": func() *harness { return open(t, theme.Default(), w, h).press("e") },
			"error": func() *harness {
				x := open(t, theme.Default(), w, h)
				x.src.Fail(errors.New(strings.Repeat("a very long reason ", 20)))
				return x.press("G").settle()
			},
			"corrupt": func() *harness {
				x := open(t, theme.Default(), w, h)
				x.src.Problem = strings.Repeat("broken ", 60)
				return x.press("v").settle()
			},
			"help": func() *harness { return open(t, theme.Default(), w, h).press("?") },
		}
		for name, build := range states {
			x := build()
			lines := strings.Split(x.frame(), "\n")
			if len(lines) != h {
				t.Errorf("%dx%d %s: frame has %d rows, want %d", w, h, name, len(lines), h)
				continue
			}
			for i, l := range lines {
				if got := ansi.StringWidth(l); got > w {
					t.Errorf("%dx%d %s: row %d is %d cells wide: %q", w, h, name, i, got, l)
				}
			}
			if !strings.Contains(lines[0], "taut") {
				t.Errorf("%dx%d %s: title bar is not on the first row", w, h, name)
			}
			capturing := x.scr.CapturingInput()
			if last := lines[h-1]; !capturing && !strings.Contains(last, "quit") {
				t.Errorf("%dx%d %s: help bar lost quit: %q", w, h, name, last)
			} else if !capturing && name != "help" && !strings.Contains(last, "help") {
				t.Errorf("%dx%d %s: help bar lost help: %q", w, h, name, last)
			}
		}
	}
}

func TestScreenResizeKeepsPlace(t *testing.T) {
	x := open(t, theme.Default(), 120, 40).press("pgdown", "down", "down")
	at := x.scr.cursor
	x.send(tea.WindowSizeMsg{Width: 60, Height: 16}).settle()
	if x.scr.cursor != at {
		t.Fatalf("cursor moved from %d to %d on resize", at, x.scr.cursor)
	}
	if at < x.scr.offset || at >= x.scr.offset+x.scr.tableHeight() {
		t.Fatalf("selected row %d is off screen (offset %d, height %d)", at, x.scr.offset, x.scr.tableHeight())
	}
	x.send(tea.WindowSizeMsg{Width: 40, Height: 10})
	if !strings.Contains(x.frame(), "too small") {
		t.Error("no too-small notice at 40x10")
	}
}

// Hostile text in an entry must not reach the terminal, in the table or in
// the detail pane, in any theme.
func TestScreenRendersNoInjectedSequences(t *testing.T) {
	x := newHarness(t, theme.Default(), 120, 40, Options{Filter: store.LogFilter{Outcome: "error", Stream: "load:C024BE91L"}}).settle()
	for _, raw := range []string{x.m.View().Content, x.press("enter").m.View().Content} {
		if strings.Contains(raw, "\x1b]") || strings.Contains(raw, "\x07") || strings.Contains(raw, "\r") {
			t.Fatalf("an injected sequence reached the frame: %q", raw)
		}
		plain := testutil.Plain(raw)
		if !strings.Contains(plain, "Insufficient privileges") {
			t.Fatalf("the detail text itself was lost:\n%s", plain)
		}
		if strings.Contains(plain, "[31m") {
			t.Fatalf("escape parameters were left behind:\n%s", plain)
		}
	}
}

func TestScreenDetailShowsEveryFieldInFull(t *testing.T) {
	x := newHarness(t, theme.Default(), 60, 30, Options{Filter: store.LogFilter{Outcome: "error", Stream: "load:C024BE91L"}}).settle().press("enter")
	e := x.scr.entries[0]
	// Wrapping may split a value across rows; compare with all layout
	// whitespace removed.
	squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
	body := squash(x.frame())
	for name, v := range map[string]string{
		"stream": e.Stream, "actor": e.Actor, "op": e.Op, "source": e.SourceID, "content hash": e.ContentHash,
		"outcome": e.Outcome, "detail": Sanitize(e.Detail), "prev hash": e.PrevHash, "entry hash": e.EntryHash,
		"time": e.At.UTC().Format(time.RFC3339Nano),
	} {
		if !strings.Contains(body, squash(v)) {
			t.Errorf("%s %q is not shown in full:\n%s", name, v, x.frame())
		}
	}
}

func TestScreenASCIIThemeEmitsOnlyASCII(t *testing.T) {
	ascii := theme.New(theme.Options{Dark: true, Unicode: false})
	for name, x := range map[string]*harness{
		"list":   open(t, ascii, 80, 24),
		"detail": open(t, ascii, 80, 24).press("enter"),
		"ops":    open(t, ascii, 80, 24).press("p"),
		"verify": open(t, ascii, 80, 24).press("v").settle(),
	} {
		for _, r := range x.frame() {
			if r > 127 {
				t.Fatalf("%s: non-ASCII rune %q in ASCII theme output", name, r)
			}
		}
	}
}

func TestScreenLightAndDarkDifferOnlyInColor(t *testing.T) {
	d := open(t, theme.New(theme.Options{Dark: true, Unicode: true}), 80, 24)
	l := open(t, theme.New(theme.Options{Dark: false, Unicode: true}), 80, 24)
	if d.m.View().Content == l.m.View().Content {
		t.Fatal("light and dark rendered identically")
	}
	if d.frame() != l.frame() {
		t.Fatal("light and dark must differ only in color")
	}
}

func TestScreenQuitAndInputCapture(t *testing.T) {
	x := open(t, theme.Default(), 80, 24)
	x.press("/").typ("q?")
	if got := x.scr.input.Value(); got != "q?" {
		t.Fatalf("typing into the filter gave %q; q or ? was taken as a command", got)
	}
	x.press("esc")
	if x.scr.text != "" || x.scr.mode != modeList {
		t.Fatal("esc did not cancel the filter edit")
	}
	_, cmd := x.m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("q produced %T, want tea.QuitMsg", cmd())
	}
}

func typeOf(v any) string { return fmt.Sprintf("%T", v) }

package shell_test

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/report"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

func doc() report.Document {
	long := func(t theme.Theme, w int) string {
		var b strings.Builder
		for i := 1; i <= 60; i++ {
			fmt.Fprintf(&b, "%s\n", ui.CheckLine(t, theme.StatusOK, fmt.Sprintf("channel-%02d", i), "1,204 messages", w))
		}
		return b.String()
	}
	return report.Document{
		Command:  "preflight",
		Subtitle: "acme-migration",
		Sections: []report.Section{
			{Title: "Overview", Render: func(t theme.Theme, w int) string {
				return ui.KV(t, [][2]string{{"Channels", "12"}, {"Messages", "5,012"}, {"Files", "3.2 GB"}})
			}},
			{Title: "Channels", Count: "60", Render: long},
			{Title: "Users", Count: "3", Status: theme.StatusWarn, Render: func(t theme.Theme, w int) string {
				return ui.CheckLine(t, theme.StatusWarn, "dana@old.example", "no Entra match", w)
			}},
		},
	}
}

func newShell(t *testing.T, th theme.Theme, w, h int) tea.Model {
	t.Helper()
	var m tea.Model = shell.New(report.NewScreen(doc()), shell.Options{
		Theme: th, Command: "preflight", Context: "acme-migration", LockTheme: true,
	})
	m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func press(m tea.Model, keys ...string) tea.Model {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		default:
			r := []rune(k)[0]
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		m, _ = m.Update(msg)
	}
	return m
}

func frame(m tea.Model) string { return m.View().Content }

func TestSnapshots(t *testing.T) {
	dark := theme.Default()
	ascii := theme.New(theme.Options{Dark: true, Unicode: false})

	cases := []struct {
		name string
		m    tea.Model
	}{
		{"overview_80x24", newShell(t, dark, 80, 24)},
		{"overview_120x40", newShell(t, dark, 120, 40)},
		{"channels_80x24", press(newShell(t, dark, 80, 24), "tab")},
		{"channels_scrolled_80x24", press(newShell(t, dark, 80, 24), "tab", "down", "down", "down")},
		{"help_80x24", press(newShell(t, dark, 80, 24), "?")},
		{"ascii_80x24", press(newShell(t, ascii, 80, 24), "tab")},
		{"too_small_40x10", newShell(t, dark, 40, 10)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			testutil.Golden(t, c.name, testutil.Plain(frame(c.m)))
		})
	}

	t.Run("error_80x24", func(t *testing.T) {
		m := newShell(t, dark, 80, 24)
		m, _ = m.Update(shell.ErrorMsg{Err: fail.New("graph.unauthorized",
			"Microsoft Graph rejected the credentials",
			"The certificate is not registered on the app registration.",
			"Run `taut auth` to re-check, or upload the certificate in the Entra portal.")})
		testutil.Golden(t, "error_80x24", testutil.Plain(frame(m)))
	})
}

// The chrome must occupy the same rows at every size and in every state, and
// every frame must be exactly the size of the window: that is what "no
// flicker, no layout jumps" means concretely.
func TestFrameIsStable(t *testing.T) {
	for _, size := range [][2]int{{60, 14}, {80, 24}, {97, 31}, {120, 40}, {200, 60}} {
		w, h := size[0], size[1]
		states := map[string]tea.Model{
			"root":     newShell(t, theme.Default(), w, h),
			"tab":      press(newShell(t, theme.Default(), w, h), "tab"),
			"help":     press(newShell(t, theme.Default(), w, h), "?"),
			"scrolled": press(newShell(t, theme.Default(), w, h), "tab", "G"),
		}
		for name, m := range states {
			lines := strings.Split(testutil.Plain(frame(m)), "\n")
			if len(lines) != h {
				t.Errorf("%dx%d %s: frame has %d rows, want %d", w, h, name, len(lines), h)
				continue
			}
			for i, l := range lines {
				if got := ansi.StringWidth(l); got > w {
					t.Errorf("%dx%d %s: row %d is %d cells wide", w, h, name, i, got)
				}
			}
			if !strings.Contains(lines[0], "taut") {
				t.Errorf("%dx%d %s: title bar is not on the first row: %q", w, h, name, lines[0])
			}
			if !strings.Contains(lines[h-1], "quit") {
				t.Errorf("%dx%d %s: help bar is not on the last row: %q", w, h, name, lines[h-1])
			}
		}
	}
}

func TestResizeKeepsState(t *testing.T) {
	m := press(newShell(t, theme.Default(), 80, 24), "tab")
	m, _ = m.Update(tea.WindowSizeMsg{Width: 132, Height: 43})
	out := testutil.Plain(frame(m))
	if !strings.Contains(out, "channel-01") {
		t.Fatalf("active section was lost on resize:\n%s", out)
	}
}

func TestASCIIThemeEmitsOnlyASCII(t *testing.T) {
	ascii := theme.New(theme.Options{Dark: true, Unicode: false})
	out := testutil.Plain(frame(press(newShell(t, ascii, 80, 24), "tab")))
	for _, r := range out {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q in ASCII theme output", r)
		}
	}
}

func TestLightAndDarkDiffer(t *testing.T) {
	d := frame(newShell(t, theme.New(theme.Options{Dark: true, Unicode: true}), 80, 24))
	l := frame(newShell(t, theme.New(theme.Options{Dark: false, Unicode: true}), 80, 24))
	if d == l {
		t.Fatal("light and dark themes rendered identically")
	}
	if testutil.Plain(d) != testutil.Plain(l) {
		t.Fatal("light and dark themes must differ only in color, not layout")
	}
}

func TestBackgroundColorSwitchesTheme(t *testing.T) {
	var m tea.Model = shell.New(report.NewScreen(doc()), shell.Options{Theme: theme.Default(), Command: "preflight"})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	before := frame(m)
	m, _ = m.Update(tea.BackgroundColorMsg{Color: whiteColor{}})
	if frame(m) == before {
		t.Fatal("shell did not restyle for a light background")
	}
}

type whiteColor struct{}

func (whiteColor) RGBA() (r, g, b, a uint32) { return 0xffff, 0xffff, 0xffff, 0xffff }

func TestQuitKeys(t *testing.T) {
	m := newShell(t, theme.Default(), 80, 24)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("q produced %T, want tea.QuitMsg", cmd())
	}
}

func TestPlainReport(t *testing.T) {
	testutil.Golden(t, "plain_report_80", testutil.Plain(doc().Sections[0].Render(theme.Default(), 80)))
}

package dashboard

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// sgr matches the colors and weights the renderer emits itself; nothing
// else may survive in a frame.
var sgr = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

func assertSafe(t *testing.T, where, out string) {
	t.Helper()
	out = sgr.ReplaceAllString(out, "")
	if !utf8.ValidString(out) {
		t.Errorf("%s: not valid UTF-8: %q", where, out)
		return
	}
	for _, r := range out {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			t.Errorf("%s: %U in %q", where, r, out)
			return
		}
	}
}

// Audit log pages reach the dashboard exactly as recorded, so the drill-in
// is where an entry is made safe to draw.
func TestDetailDrawsAuditLogEntriesSafely(t *testing.T) {
	bidi := string(rune(0x202e))
	for name, p := range map[string]string{
		"osc title": "\x1b]0;x\x07", "clear": "\x1b[2J", "osc 52": "\x1b]52;c;AAAA\x07", "dcs": "\x1bPq#0\x1b\\",
		"carriage return": "\r", "c1 csi": string(rune(0x9b)) + "2J", "raw c1": "\x9b2J", "bidi": bidi, "unterminated": "\x1b]0;x",
	} {
		t.Run(name, func(t *testing.T) {
			lane := "load:Cincidents"
			entries := []store.LogEntry{
				{Stream: lane, Seq: 1, At: t0.Add(-30 * time.Second), Op: "message" + p + ".import", SourceID: "Cincidents/" + p + "1699000000.000100",
					TargetID: "1699" + p + "000", Outcome: "ok", Detail: "fine" + p},
				{Stream: lane, Seq: 2, At: t0.Add(-20 * time.Second), Op: "message.fail" + p, SourceID: p + "Cincidents/1699000050.000300", Outcome: "error",
					Detail: "413 Request Entity Too Large:" + p + " the message body is 4.2 MB\nsecond line" + p + "\r\nthird"},
			}
			r := running(t, 140, 40)
			r.src.entries = entries
			r.press("3", "enter")
			if r.s.detail == nil || len(r.s.detail.errs) != 1 || len(r.s.detail.recent) != 1 {
				t.Fatalf("the drill-in did not load the entries: %+v", r.s.detail)
			}
			if got := r.s.detail.errs[0].Detail; got != entries[1].Detail {
				t.Errorf("the entry itself was altered: %q", got)
			}
			lines := strings.Join(r.s.detailLines(120), "\n")
			assertSafe(t, "drill-in lines", lines)
			for _, want := range []string{"413 Request Entity Too Large:", "second line", "third", "Cincidents/"} {
				if !strings.Contains(ansi.Strip(lines), want) {
					t.Errorf("the drill-in lost %q:\n%s", want, ansi.Strip(lines))
				}
			}
			assertSafe(t, "drill-in frame", r.m.View().Content)
		})
	}
}

// swamped is a snapshot as an attacker on the bus could make the controller report
// it: far more budgets and throttles than a cluster has, with names far
// longer than a tenant ID or a scope.
func swamped(step int) controller.DashboardView {
	v := midRun(step)
	holders := make([]string, 4096)
	for i := range holders {
		holders[i] = strings.Repeat("h", 256)
	}
	v.Cluster.Budgets, v.Cluster.Throttles = nil, nil
	for i := range 300 {
		v.Cluster.Budgets = append(v.Cluster.Budgets, budget.KeyStatus{
			Key:      budget.Key{Tenant: fmt.Sprintf("%04d", i) + strings.Repeat("t", 4000), Scope: fmt.Sprintf("%03d", i) + strings.Repeat("s", 10000)},
			TotalRPS: 1e300, ShareRPS: 1e300 / 4096, Holders: holders,
		})
		v.Cluster.Throttles = append(v.Cluster.Throttles, controller.ThrottleView{
			Tenant: strings.Repeat("t", 4000), Scope: fmt.Sprintf("%03d", i) + strings.Repeat("s", 10000), Holder: strings.Repeat("h", 256),
			At: v.At.Add(-time.Second), Until: v.At.Add(time.Minute),
		})
	}
	return v
}

func TestClusterPaneIsBounded(t *testing.T) {
	th := theme.Default()
	for _, sz := range [][2]int{{140, 40}, {80, 24}, {40, 12}} {
		r := newHarness(t, &fakeSource{views: script(4, swamped)}, th, sz[0], sz[1])
		r.poll(2)
		r.press("2") // focus the cluster pane, so it is drawn at every size
		for i, l := range strings.Split(r.m.View().Content, "\n") {
			if w := ansi.StringWidth(l); w > sz[0] {
				t.Errorf("%dx%d: row %d is %d wide", sz[0], sz[1], i, w)
			}
		}

		lines := r.s.clusterLines(sz[0])
		rows, more := 0, false
		for _, l := range lines {
			plain := ansi.Strip(l)
			if strings.Contains(plain, "proxies") {
				rows++
				if !strings.Contains(plain, "4,096 proxies") {
					t.Errorf("a budget row does not count its holders: %q", plain)
				}
				if strings.Contains(plain, "hhhh") {
					t.Errorf("a budget row lists its holders: %.80q", plain)
				}
			}
			more = more || strings.Contains(plain, "292 more")
			if w := ansi.StringWidth(l); w > 400 {
				t.Errorf("%dx%d: a cluster line is %d cells wide before clipping: %.80q", sz[0], sz[1], w, plain)
			}
		}
		if rows != maxBudgetRows || !more {
			t.Errorf("%dx%d: %d budget rows drawn (want %d), remainder noted: %v", sz[0], sz[1], rows, maxBudgetRows, more)
		}
		if _, text := r.s.throttleText(); len(text) > 400 || strings.Count(text, ",") != maxThrottleScopes || !strings.Contains(text, "292 more") {
			t.Errorf("throttle line is not capped (%d bytes): %.120q", len(text), text)
		}
	}

	for _, row := range Summary(th, swamped(0)) {
		if len(row[1]) > 1200 {
			t.Errorf("summary row %q is %d bytes", row[0], len(row[1]))
		}
		if row[0] == "Budgets" && (strings.Count(row[1], "rps across") != maxBudgetRows || !strings.Contains(row[1], "292 more")) {
			t.Errorf("summary budgets are not capped: %.200q", row[1])
		}
	}

	// A cluster of ordinary size is drawn in full, with no remainder line.
	r := running(t, 140, 40)
	if got := strings.Join(r.s.clusterLines(60), "\n"); strings.Contains(got, "more") || strings.Count(got, "prox") != 2 {
		t.Errorf("an ordinary cluster was capped:\n%s", ansi.Strip(got))
	}
}

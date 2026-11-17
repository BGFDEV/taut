// Package testutil holds helpers shared by tests.
package testutil

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

var update = flag.Bool("update", false, "rewrite golden files instead of comparing")

// Golden compares got with testdata/<name>.golden, or rewrites the file when
// the test is run with -update. Trailing spaces are trimmed from each line so
// snapshots stay stable in editors and diffs.
func Golden(t *testing.T, name, got string) {
	t.Helper()
	got = normalize(got)
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file %s (run `go test -update` to create it): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden.\n--- want\n%s\n--- got\n%s\n(run `go test -update` if the change is intended)", name, want, got)
	}
}

var elapsed = regexp.MustCompile(` · \d+(\.\d+)?s\b|\b\d+m \d+s\b`)

// Stable removes elapsed times, which vary from run to run, so output that
// reports them can be compared with a golden file.
func Stable(s string) string { return elapsed.ReplaceAllString(s, "") }

// Plain strips ANSI escape sequences, leaving only what is visible.
func Plain(s string) string { return ansi.Strip(s) }

func normalize(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}

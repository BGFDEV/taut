package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A local-mode config file says nothing about a bucket or a controller,
// round-trips, and is valid without either.
func TestLocalMode(t *testing.T) {
	m := example(t)
	m.Local = Local{Enabled: true, StateDir: "state", Postgres: LocalPostgres{Download: true}}
	// What a cluster would need and local mode does not.
	m.Staging.Bucket, m.Controller.URL = "", ""
	if issues := m.Validate(); issues.HasErrors() {
		t.Fatalf("a local-mode config file with no bucket and no controller is refused: %v", issues.Err())
	}

	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, absent := range []string{"staging:", "controller:", "TAUT_S3_ACCESS_KEY"} {
		if strings.Contains(text, absent) {
			t.Errorf("the local-mode file mentions %q:\n%s", absent, text)
		}
	}
	for _, want := range []string{"local:\n  enabled: true\n  stateDir: state\n  postgres:\n    download: true", "# Local mode:"} {
		if !strings.Contains(text, want) {
			t.Errorf("the local-mode file lacks %q:\n%s", want, text)
		}
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal()): %v\n%s", err, data)
	}
	if !reflect.DeepEqual(back.Local, m.Local) {
		t.Errorf("local = %+v after a round trip, want %+v", back.Local, m.Local)
	}
	if issues := back.Validate(); issues.HasErrors() {
		t.Errorf("the round-tripped file is not valid: %v", issues.Err())
	}

	// The state directory is beside the config file unless it says
	// otherwise.
	m.SetBaseDir("/srv/migration")
	if got := m.StateDir(); got != filepath.FromSlash("/srv/migration/state") {
		t.Errorf("StateDir = %q", got)
	}
	m.Local.StateDir = ""
	if got := m.StateDir(); got != filepath.FromSlash("/srv/migration/.taut") {
		t.Errorf("default StateDir = %q", got)
	}
	m.Local.StateDir = "/var/lib/taut"
	if got := m.StateDir(); got != filepath.FromSlash("/var/lib/taut") {
		t.Errorf("absolute StateDir = %q", got)
	}
}

func TestLocalModeValidation(t *testing.T) {
	// A cluster config file is unchanged: no local section is written, and
	// the bucket and the controller are still required.
	m := example(t)
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "local:") {
		t.Errorf("a cluster config file has a local section:\n%s", data)
	}
	m.Controller.URL = ""
	if issues := m.Validate(); !issues.HasErrors() {
		t.Error("a cluster config file with no controller is accepted")
	}

	// Settings under local without enabling it are pointed out.
	m = example(t)
	m.Local.StateDir = "state"
	warned := false
	for _, w := range m.Validate().Warnings() {
		warned = warned || w.Path == "local"
	}
	if !warned {
		t.Error("local settings that have no effect are not warned about")
	}

	// A named installation and a download are one too many.
	m = example(t)
	m.Local = Local{Enabled: true, Postgres: LocalPostgres{Binaries: "/usr/lib/postgresql/17", Download: true}}
	warned = false
	for _, w := range m.Validate().Warnings() {
		warned = warned || w.Path == "local.postgres.download"
	}
	if !warned {
		t.Error("download beside a named installation is not warned about")
	}

	// Unknown keys under local are refused like anywhere else.
	if _, err := Parse([]byte(strings.Replace(string(mustLocal(t)), "enabled: true", "enabled: true\n  workers: 3", 1))); err == nil {
		t.Error("an unknown key under local was accepted")
	}
}

func mustLocal(t *testing.T) []byte {
	t.Helper()
	m := example(t)
	m.Local.Enabled = true
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

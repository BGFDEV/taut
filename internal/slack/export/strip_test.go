package export_test

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
)

// A copy without direct messages has none of them: not their index files,
// not their folders, not the folders' names. Every channel is as it was.
func TestStripDirectMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []fixtures.Option
	}{
		{"plain", nil},
		{"wrapped", []fixtures.Option{fixtures.Wrapped("Fixture Slack export Mar 4 2024")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, err := fixtures.Write("medium", t.TempDir(), tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if has, err := export.HasDirectMessages(src); err != nil || !has {
				t.Fatalf("the fixture has no direct messages: %v %v", has, err)
			}
			before := open(t, src)
			all, err := before.Channels()
			if err != nil {
				t.Fatal(err)
			}
			private := map[string]bool{}
			var channels []slack.Channel
			for _, ch := range all {
				if ch.Kind == slack.KindDM || ch.Kind == slack.KindMPIM {
					private[ch.Folder()] = true
				} else {
					channels = append(channels, ch)
				}
			}
			if len(private) == 0 || len(channels) == 0 {
				t.Fatalf("the fixture has %d direct messages and %d channels", len(private), len(channels))
			}

			dst := filepath.Join(t.TempDir(), "stripped.zip")
			f, err := os.Create(dst)
			if err != nil {
				t.Fatal(err)
			}
			removed, err := export.StripDirectMessages(src, f)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if removed == 0 {
				t.Fatal("nothing was left out")
			}

			zr, err := zip.OpenReader(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer zr.Close()
			for _, e := range zr.File {
				for _, part := range strings.Split(strings.Trim(e.Name, "/"), "/") {
					if part == "dms.json" || part == "mpims.json" || private[part] {
						t.Errorf("the copy still has %s", e.Name)
					}
				}
			}
			if has, err := export.HasDirectMessages(dst); err != nil || has {
				t.Errorf("the copy still has direct messages: %v %v", has, err)
			}

			// The channels read exactly as before.
			after := open(t, dst)
			got, err := after.Channels()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, channels) {
				t.Errorf("the copy lists %d channels, the export %d", len(got), len(channels))
			}
			for _, ch := range channels {
				want, _ := before.Days(ch)
				days, err := after.Days(ch)
				if err != nil || !reflect.DeepEqual(days, want) {
					t.Errorf("%s: days %v, want %v (%v)", ch.Name, days, want, err)
				}
				for _, day := range days {
					a, b := read(t, before, ch.Folder()+"/"+day+".json"), read(t, after, ch.Folder()+"/"+day+".json")
					if !bytes.Equal(a, b) {
						t.Errorf("%s/%s differs in the copy", ch.Name, day)
					}
				}
			}
			wantUsers, _ := before.Users()
			if users, err := after.Users(); err != nil || !reflect.DeepEqual(users, wantUsers) {
				t.Errorf("the users differ in the copy: %v", err)
			}
		})
	}
}

func read(t *testing.T, e *export.Export, name string) []byte {
	t.Helper()
	f, err := e.FS().Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A folder no index lists, a second spelling of a path, and what an archive
// tool put beside the export do not get past.
func TestStripDirectMessagesKeepsOnlyChannels(t *testing.T) {
	src := filepath.Join(t.TempDir(), "export.zip")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{
		"export/users.json":                       `[]`,
		"export/channels.json":                    `[{"id":"C1","name":"general"}]`,
		"export/dms.json":                         `[{"id":"D1","members":["U1","U2"]}]`,
		"export/mpims.json":                       `[{"id":"G1","name":"mpdm-ada--ben--cy-1","members":["U1","U2","U3"]}]`,
		"export/general/2024-01-01.json":          `[]`,
		"export/D1/2024-01-01.json":               `[{"text":"private"}]`,
		"export/mpdm-ada--ben--cy-1/":             ``,
		"export/mpdm-ada--ben--cy-1/2024-01.json": `[{"text":"private"}]`,
		"export/D9/2024-01-01.json":               `[{"text":"listed nowhere"}]`,
		"export//D1/2024-01-02.json":              `[{"text":"private"}]`,
		`export\D1\2024-01-03.json`:               `[{"text":"private"}]`,
		"export/./dms.json":                       `[]`,
		"__MACOSX/export/D1/._2024-01-01.json":    `x`,
		"export/integration_logs.json":            `[]`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := export.StripDirectMessages(src, &out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range zr.File {
		names = append(names, e.Name)
	}
	want := map[string]bool{"export/users.json": true, "export/channels.json": true, "export/general/2024-01-01.json": true, "export/integration_logs.json": true}
	if len(names) != len(want) {
		t.Errorf("the copy holds %v", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("the copy holds %s", n)
		}
	}
}

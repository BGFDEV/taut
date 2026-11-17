package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/mapping"
	"github.com/bgfdev/taut/internal/tui/theme"
)

const mapYAML = `# Hand-written plan.
apiVersion: taut.dev/v1alpha1
kind: Config
metadata: {name: acme}
source:
  slack:
    export: export.zip   # from the March export
target:
  entra:
    tenantId: 7f3c1e9a-52d4-4b1e-9a57-0c8f2d6e4b10
    clientId: 2b9d4c1f-8e3a-4f6b-b7c2-5a1e0d9f3c84
    credential:
      clientSecret: {env: TAUT_ENTRA_CLIENT_SECRET}
  teams:
    - name: Acme
      owner: admin@acme.example
      channels:
        include:
          - "*"
        exclude:
          - tmp-*      # scratch channels
          - sales-*
          - random     # agreed with HR
        rename:
          dev: Engineering   # their choice
    # the second team is written compactly
    - name: Sales
      owner: admin@acme.example
      channels: {include: [sales-*]}
identity:
  unmapped:
    account: former@acme.example
`

func mapFixture(t *testing.T) (*configFile, *mapping.Editor) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "taut.yaml")
	if err := os.WriteFile(path, []byte(mapYAML), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := openConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := f.loaded
	var channels []slack.Channel
	for i, n := range []string{"general", "random", "dev", "design", "tmp-1", "sales-emea"} {
		channels = append(channels, slack.Channel{ID: "C" + string(rune('A'+i)), Name: n, Kind: slack.KindPublic, IsGeneral: n == "general"})
	}
	return f, mapping.NewEditor(m, channels, nil, nil)
}

// A hand-written config file keeps its comments, its layout choices and its
// file mode; only the entries that changed are touched.
func TestMapSavePatchesInPlace(t *testing.T) {
	f, ed := mapFixture(t)
	for _, err := range []error{
		ed.Include("random", ""), ed.Exclude("sales-emea"), ed.Rename("dev", "Platform"),
		ed.Rename("design", "Studio"), ed.Move("design", "Sales"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.save(ed.Config()); err != nil {
		t.Fatal(err)
	}
	if f.normalized {
		t.Fatal("the save fell back to a full rewrite")
	}
	raw, _ := os.ReadFile(f.path)
	testutil.Golden(t, "map_patched_config", string(raw))

	back, err := config.Load(f.path)
	if err != nil || !back.Equal(ed.Config()) {
		t.Fatalf("the patched file is not the edited config file: %v", err)
	}
	for _, want := range []string{"# Hand-written plan.", "# from the March export", "# scratch channels", "# their choice", "# the second team is written compactly"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("comment %q was lost", want)
		}
	}
	if strings.Contains(string(raw), "agreed with HR") {
		t.Error("the comment on a removed entry should go with it")
	}
	if st, _ := os.Stat(f.path); st.Mode().Perm() != 0o640 {
		t.Errorf("file mode changed to %v", st.Mode().Perm())
	}

	// A second save in the same session builds on the first.
	if err := ed.Exclude("dev"); err != nil {
		t.Fatal(err)
	}
	if err := f.save(ed.Config()); err != nil {
		t.Fatal(err)
	}
	if back, err := config.Load(f.path); err != nil || !back.Equal(ed.Config()) {
		t.Fatalf("second save: %v", err)
	}
}

func TestMapSaveRefusesToOverwriteAForeignEdit(t *testing.T) {
	f, ed := mapFixture(t)
	if err := ed.Exclude("design"); err != nil {
		t.Fatal(err)
	}
	foreign := mapYAML + "# edited by someone else\n"
	if err := os.WriteFile(f.path, []byte(foreign), 0o640); err != nil {
		t.Fatal(err)
	}
	err := f.save(ed.Config())
	if fe := fail.From(err); err == nil || fe.Code != "map.config_changed" || fe.Next == "" {
		t.Fatalf("save = %v", err)
	}
	if raw, _ := os.ReadFile(f.path); string(raw) != foreign {
		t.Fatal("the foreign edit was overwritten")
	}
}

// A document the patcher cannot edit safely is rewritten in the standard
// form rather than risked, and the operator is told.
func TestMapSaveFallsBackToTheStandardForm(t *testing.T) {
	f, _ := mapFixture(t)
	aliased := strings.Replace(mapYAML, "owner: admin@acme.example\n      channels:", "owner: &owner admin@acme.example\n      <<: {visibility: private}\n      channels:", 1)
	if err := os.WriteFile(f.path, []byte(aliased), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openConfigFile(f.path)
	if err != nil {
		t.Skipf("the parser rejects merge keys, so the fallback cannot be reached this way: %v", err)
	}
	m := f.loaded
	e2 := mapping.NewEditor(m, []slack.Channel{{ID: "C1", Name: "design", Kind: slack.KindPublic}}, nil, nil)
	if err := e2.Exclude("design"); err != nil {
		t.Fatal(err)
	}
	if err := f.save(e2.Config()); err != nil {
		t.Fatal(err)
	}
	back, err := config.Load(f.path)
	if err != nil || !back.Equal(e2.Config()) {
		t.Fatalf("fallback save is not the edited config file: %v", err)
	}
	if !f.normalized {
		t.Fatal("the fallback was not reported")
	}
}

func TestMapSessionSummary(t *testing.T) {
	th := theme.Default()
	render := func(f *configFile, ed *mapping.Editor, saves int) string {
		return testutil.Plain(mapSessionSummary(th, ed, saves, f, 78))
	}

	f, ed := mapFixture(t)
	out := render(f, ed, 0)
	out += "\n---\n"

	_ = ed.Exclude("design")
	_ = ed.Rename("dev", "Platform")
	out += render(f, ed, 0) + "\n---\n"

	if err := f.save(ed.Config()); err != nil {
		t.Fatal(err)
	}
	ed.MarkSaved()
	out += render(f, ed, 1) + "\n---\n"

	_ = ed.Include("random", "")
	out += render(f, ed, 1)
	testutil.Golden(t, "map_session_summary", out)
}

// A team with no channels block (every default) gains one when edited.
func TestMapSaveAddsAChannelsBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taut.yaml")
	src := mapYAML[:strings.Index(mapYAML, "      channels:")] + mapYAML[strings.Index(mapYAML, "identity:"):]
	src = strings.Replace(src, "    - name: Acme\n      owner: admin@acme.example\n", "    - name: Acme # the only team\n      owner: admin@acme.example\n", 1)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ed := mapping.NewEditor(f.loaded, []slack.Channel{{ID: "C1", Name: "random", Kind: slack.KindPublic}, {ID: "C2", Name: "dev", Kind: slack.KindPublic}}, nil, nil)
	if err := ed.Exclude("random"); err != nil {
		t.Fatal(err)
	}
	if err := ed.Rename("dev", "Engineering"); err != nil {
		t.Fatal(err)
	}
	if err := f.save(ed.Config()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	back, err := config.Load(path)
	if err != nil || !back.Equal(ed.Config()) || f.normalized {
		t.Fatalf("err=%v normalized=%v\n%s", err, f.normalized, raw)
	}
	if !strings.Contains(string(raw), "# the only team") || !strings.Contains(string(raw), "exclude: [random]") {
		t.Fatalf("unexpected file:\n%s", raw)
	}
}

// A config file reached through a symbolic link is written where the link
// points: the link stays a link and the real file is the one that changes.
func TestMapSaveWritesThroughASymlink(t *testing.T) {
	realDir, linkDir := t.TempDir(), t.TempDir()
	realPath, link := filepath.Join(realDir, "shared.yaml"), filepath.Join(linkDir, "taut.yaml")
	if err := os.WriteFile(realPath, []byte(mapYAML), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, link); err != nil {
		t.Skipf("symbolic links are not available here: %v", err)
	}
	f, err := openConfigFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if f.name != "taut.yaml" {
		t.Errorf("the file is shown as %q, not by the name the operator used", f.name)
	}
	ed := mapping.NewEditor(f.loaded, []slack.Channel{{ID: "C1", Name: "design", Kind: slack.KindPublic}}, nil, nil)
	for i, edit := range []func() error{
		func() error { return ed.Exclude("design") },
		func() error { return ed.Include("design", "Acme") }, // a second save in the same session
	} {
		if err := edit(); err != nil {
			t.Fatal(err)
		}
		if err := f.save(ed.Config()); err != nil {
			t.Fatalf("save %d: %v", i+1, err)
		}
		st, err := os.Lstat(link)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("save %d replaced the link with a file (mode %v, %v)", i+1, st.Mode(), err)
		}
		if dest, _ := os.Readlink(link); dest != realPath {
			t.Fatalf("save %d pointed the link at %q", i+1, dest)
		}
		back, err := config.Load(realPath)
		if err != nil || !back.Equal(ed.Config()) {
			t.Fatalf("save %d: the real file is not the edited config file: %v", i+1, err)
		}
	}
	if st, _ := os.Stat(realPath); st.Mode().Perm() != 0o640 {
		t.Errorf("the real file's mode changed to %v", st.Mode().Perm())
	}
	for _, dir := range []string{realDir, linkDir} {
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Errorf("%s holds %d entries after saving; temporary files were left behind or written beside the link", dir, len(entries))
		}
	}
}

// The config file map edits and the bytes a save compares the file against
// come from one read. Were they read separately, a change landing between
// the two would be in the baseline but not in the config file, and the save
// would pass the guard and undo it.
func TestMapSaveBaselineIsWhatWasParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taut.yaml")
	if err := os.WriteFile(path, []byte(mapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Someone else's edit, arriving as soon as the file has been read: it
	// takes #design out of the migration.
	foreign := strings.Replace(mapYAML, "          - random     # agreed with HR\n", "          - random     # agreed with HR\n          - design\n", 1)
	if foreign == mapYAML {
		t.Fatal("the fixture no longer has the line this test edits")
	}
	if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}

	if string(f.data) != mapYAML {
		t.Fatal("the baseline is not the bytes that were on disk when the config file was loaded")
	}
	if back, err := config.Parse(f.data); err != nil || !back.Equal(f.loaded) {
		t.Fatalf("the baseline does not parse to the config file being edited: %v", err)
	}

	ed := mapping.NewEditor(f.loaded, []slack.Channel{{ID: "C1", Name: "dev", Kind: slack.KindPublic}}, nil, nil)
	if err := ed.Rename("dev", "Platform"); err != nil {
		t.Fatal(err)
	}
	err = f.save(ed.Config())
	if fe := fail.From(err); err == nil || fe.Code != "map.config_changed" || fe.Next == "" {
		t.Fatalf("save = %v, want map.config_changed", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != foreign {
		t.Fatalf("the other edit was overwritten:\n%s", raw)
	}
}

// taut reads only the first YAML document of a file. Saving would write
// that one back and drop the rest, so map refuses, saying what to do.
func TestMapSaveRefusesAMultiDocumentFile(t *testing.T) {
	for name, tail := range map[string]string{
		"a second document":    "---\nkind: Something\nelse: true\n",
		"an empty document":    "---\n",
		"a commented document": "---\n# notes kept under the config file\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "taut.yaml")
			src := mapYAML + tail
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			// Reading is unaffected: the mapping can still be shown.
			f, err := openConfigFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ed := mapping.NewEditor(f.loaded, []slack.Channel{{ID: "C1", Name: "design", Kind: slack.KindPublic}}, nil, nil)
			if err := ed.Exclude("design"); err != nil {
				t.Fatal(err)
			}
			err = f.save(ed.Config())
			fe := fail.From(err)
			if err == nil || fe.Code != "map.multi_document" || fe.Exit != fail.ExitUsage {
				t.Fatalf("save = %v (exit %d), want map.multi_document with exit %d", err, fe.Exit, fail.ExitUsage)
			}
			if !strings.Contains(fe.Next, "---") || !strings.Contains(fe.Next, "taut.yaml") || !strings.Contains(fe.Next, "taut map") {
				t.Errorf("the next step is not exact: %q", fe.Next)
			}
			if raw, _ := os.ReadFile(path); string(raw) != src {
				t.Fatal("the file was written")
			}
		})
	}
	// A leading document marker is still one document.
	path := filepath.Join(t.TempDir(), "taut.yaml")
	if err := os.WriteFile(path, []byte("---\n"+mapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.editable(); err != nil {
		t.Fatalf("a single document with a leading marker was refused: %v", err)
	}
}

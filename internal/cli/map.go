package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/tui/mapping"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// mapEdits are the scripted edits given as flags.
type mapEdits struct {
	exclude, include, rename, move []string
}

func (e mapEdits) any() bool {
	return len(e.exclude)+len(e.include)+len(e.rename)+len(e.move) > 0
}

// mapDocument is the `taut map --json` result. Field names are a
// contract; add, never rename.
type mapDocument struct {
	Config string `json:"config"`
	*mapping.View
	// Changes lists what the edit flags changed in the mapping.
	Changes []mapping.Change `json:"changes"`
	// Saved is true when the config file was written.
	Saved bool `json:"saved"`
}

func newMapCmd(app *App) *cobra.Command {
	var edits mapEdits
	cmd := &cobra.Command{
		Use:     "map",
		Short:   "Review and edit the Slack → Teams mapping",
		GroupID: groupPlan,
		Long: `Review the mapping. map shows which Slack channel goes to which team under
which name, exactly as the migration will do it, and lets you change it:
leave channels out, bring them back, rename them, or move them between teams.

On a terminal it opens an editable tree. Piped, or with --json, it prints the
mapping. The edit flags make the same changes from a script; they are applied
in the order include, exclude, move, rename, and the config file is written only
with --yes. Without --yes map shows what would change and writes nothing.

Direct messages are listed when the config file migrates them
(target.directMessages.enabled), each with whether it can go into a Teams
chat and, if not, why. They cannot be edited: who is in a conversation
decides. map sees only what the export says about its participants; whether
each has a Microsoft 365 account is checked by taut preflight.

map reads the Slack export only. It never contacts Slack or Microsoft, and
it changes nothing in the config file except the channel selection of its teams.
It exits 3, writing nothing, if the edits leave a blocker.`,
		Example: `  taut map
  taut map --json > mapping.json
  taut map --exclude random --exclude 'tmp-old' --yes
  taut map --rename dev=Engineering --move sales='Go To Market' --yes
  taut map --include leadership=Management`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			io := app.IO
			// One read gives both the config file that is edited and the bytes
			// a save compares the file against.
			file, err := openConfigFile(app.ConfigPath)
			if err != nil {
				return err
			}
			m := file.loaded
			if io.Interactive() || edits.any() {
				// Refuse now rather than after the edits have been made.
				if err := file.editable(); err != nil {
					return err
				}
			}
			channels, users, days, err := mapChannels(m)
			if err != nil {
				return err
			}
			// Existing team names would need a directory read from
			// Microsoft Graph; map stays offline and leaves that
			// collision check to `taut preflight`.
			ed := mapping.NewEditor(m, channels, nil, days)
			ed.SetUsers(users)

			if io.Interactive() && !edits.any() {
				scr := mapping.NewScreen(ed, mapping.Options{Save: file.save, File: file.name})
				if err := shell.Run(cmd.Context(), io, scr, shell.Options{Command: "map", Context: m.Metadata.Name}); err != nil {
					return err
				}
				// Leave the outcome in the scrollback after the
				// full-screen view closes.
				io.Println("\n" + ui.Pad(mapSessionSummary(io.Theme, ed, scr.Saves(), file, io.Width-ui.Margin)))
				return nil
			}

			if err := edits.apply(ed); err != nil {
				return err
			}
			view := ed.View()
			changes := ed.Unsaved()
			if changes == nil {
				changes = []mapping.Change{}
			}
			blocked := len(view.Blockers) > 0
			saved := false
			if edits.any() && app.Yes && ed.Dirty() && !blocked {
				if err := file.save(ed.Config()); err != nil {
					return err
				}
				saved = true
			}

			if io.IsJSON() {
				if err := io.JSON(mapDocument{Config: m.Metadata.Name, View: view, Changes: changes, Saved: saved}); err != nil {
					return err
				}
			} else {
				io.Println(mapPlain(io.Theme, m.Metadata.Name, view, changes, mapOutcome{
					edits: edits.any(), saved: saved, dirty: ed.Dirty(), file: file, retry: edits.command(app.ConfigPath),
				}, io.Width))
			}
			if blocked && edits.any() {
				return fail.Exit(fail.ExitBlockers, "the edited mapping has blockers")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&edits.exclude, "exclude", nil, "leave a Slack channel out of the migration (repeatable)")
	f.StringArrayVar(&edits.include, "include", nil, "migrate a channel that is left out: <channel> or <channel>=<team name> (repeatable)")
	f.StringArrayVar(&edits.rename, "rename", nil, "name a channel in Teams: <slack-name>=<teams-name>; an empty name resets it (repeatable)")
	f.StringArrayVar(&edits.move, "move", nil, "send a channel to another team: <slack-name>=<team name> (repeatable)")
	return cmd
}

// mapChannels reads the channel list and, per channel, the number of days
// with messages. Both come from the export's index and folder listing, so no
// message is read.
func mapChannels(m *config.Config) ([]slack.Channel, []slack.User, map[string]int, error) {
	ex, err := export.Open(m.ResolvePath(m.Source.Slack.Export))
	if err != nil {
		return nil, nil, nil, err
	}
	defer ex.Close()
	channels, err := ex.Channels()
	if err != nil {
		return nil, nil, nil, err
	}
	// The users say what each participant of a direct message is in Slack.
	// They are only needed when the config file migrates direct messages.
	var users []slack.User
	if m.Target.DirectMessages.Enabled {
		if users, err = ex.Users(); err != nil {
			return nil, nil, nil, err
		}
	}
	days := map[string]int{}
	for _, ch := range channels {
		if ch.Kind == slack.KindDM || ch.Kind == slack.KindMPIM {
			continue
		}
		// A channel whose folder cannot be listed shows no count.
		if d, err := ex.Days(ch); err == nil {
			days[ch.ID] = len(d)
		}
	}
	return channels, users, days, nil
}

func splitPair(flag, v string) (left, right string, err error) {
	left, right, ok := strings.Cut(v, "=")
	left = strings.TrimSpace(left)
	if !ok || left == "" {
		return "", "", fail.Usagef(fmt.Sprintf("Write it as --%s <slack-name>=<value>, quoting it if the value has spaces.", flag),
			"--%s %q is not of the form name=value", flag, v)
	}
	return left, strings.TrimSpace(right), nil
}

// apply runs the scripted edits. An edit that changes nothing (excluding a
// channel that is already out) is not an error, so scripts can be re-run.
func (e mapEdits) apply(ed *mapping.Editor) error {
	ok := func(err error) error {
		if errors.Is(err, mapping.ErrNoChange) {
			return nil
		}
		return err
	}
	for _, v := range e.include {
		name, team, _ := strings.Cut(v, "=")
		if err := ok(ed.Include(name, strings.TrimSpace(team))); err != nil {
			return err
		}
	}
	for _, v := range e.exclude {
		if err := ok(ed.Exclude(v)); err != nil {
			return err
		}
	}
	for _, v := range e.move {
		name, team, err := splitPair("move", v)
		if err != nil {
			return err
		}
		if err := ok(ed.Move(name, team)); err != nil {
			return err
		}
	}
	for _, v := range e.rename {
		name, to, err := splitPair("rename", v)
		if err != nil {
			return err
		}
		if err := ok(ed.Rename(name, to)); err != nil {
			return err
		}
	}
	return nil
}

// command rebuilds the command line with --yes, for the Next hint.
func (e mapEdits) command(configPath string) string {
	quote := func(s string) string {
		if s != "" && !strings.ContainsAny(s, " \t'\"\\$`!*?[]{}()<>|&;#~") {
			return s
		}
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	parts := []string{"taut", "map"}
	for _, g := range []struct {
		flag string
		vals []string
	}{{"include", e.include}, {"exclude", e.exclude}, {"move", e.move}, {"rename", e.rename}} {
		for _, v := range g.vals {
			parts = append(parts, "--"+g.flag, quote(v))
		}
	}
	if configPath != "" && configPath != config.DefaultPath {
		parts = append(parts, "--config", quote(configPath))
	}
	return strings.Join(append(parts, "--yes"), " ")
}

// --- rendering ----------------------------------------------------------------

type mapOutcome struct {
	edits, saved, dirty bool
	file                *configFile
	retry               string
}

func mapPlain(t theme.Theme, name string, v *mapping.View, changes []mapping.Change, o mapOutcome, width int) string {
	inner := width - ui.Margin
	var b strings.Builder
	b.WriteString("\n" + ui.Banner(t, "map", name) + "\n\n")
	b.WriteString(v.Plain(t, inner))
	if len(changes) > 0 {
		b.WriteString("\n\n" + ui.HeadingNote(t, "Changes", fmt.Sprint(len(changes))) + "\n")
		b.WriteString(ui.Indent(mapping.ChangeList(t, changes, inner-2), 2))
	}
	if f := v.Findings(t, inner); f != "" {
		b.WriteString("\n\n" + f)
	}
	b.WriteString("\n\n" + mapVerdict(t, v, len(changes), o, inner))
	return ui.Pad(b.String())
}

func mapVerdict(t theme.Theme, v *mapping.View, changes int, o mapOutcome, width int) string {
	next := ui.Next(t, "taut preflight", "check this plan")
	blockers := countOf(len(v.Blockers), "blocker", "blockers")
	switch {
	case o.edits && len(v.Blockers) > 0:
		return ui.Verdict(t, theme.StatusFail, "Not saved", blockers+" in the edited mapping") + "\n" +
			ui.Hint(t, "Nothing was written. Resolve the blockers above in the same command, then run it again.", width)
	case o.saved:
		out := ui.Verdict(t, theme.StatusOK, "Saved "+countOf(changes, "change", "changes")+" to "+o.file.name, v.Summary())
		if o.file.normalized {
			out += "\n" + ui.Hint(t, "The file's comments and layout could not be kept and were rewritten in the standard form.", width)
		}
		return out + "\n\n" + next
	case o.edits && o.dirty:
		return ui.Verdict(t, theme.StatusWarn, "Not saved", countOf(changes, "change", "changes")+" pending", o.file.name+" is untouched") +
			"\n\n" + ui.Next(t, o.retry, "write the changes")
	case o.edits:
		return ui.Verdict(t, theme.StatusOK, "Nothing to change", "the config file already says this", v.Summary()) + "\n\n" + next
	case len(v.Blockers) > 0:
		return ui.Verdict(t, theme.StatusFail, blockers, v.Summary()) + "\n" +
			ui.Hint(t, "Fix the blockers above with `taut map`, then run `taut preflight`.", width)
	case len(v.Warnings) > 0:
		return ui.Verdict(t, theme.StatusWarn, v.Summary(), countOf(len(v.Warnings), "warning", "warnings")) + "\n\n" + next
	}
	return ui.Verdict(t, theme.StatusOK, v.Summary()) + "\n\n" + next
}

// mapSessionSummary is what an interactive session leaves in scrollback:
// what was saved, as a diff, one verdict line and the next command.
func mapSessionSummary(t theme.Theme, ed *mapping.Editor, saves int, file *configFile, width int) string {
	saved, lost := ed.Saved(), ed.Unsaved()
	v := ed.View()
	var b strings.Builder
	if len(saved) > 0 {
		b.WriteString(ui.HeadingNote(t, "Changes", fmt.Sprint(len(saved))) + "\n")
		b.WriteString(ui.Indent(mapping.ChangeList(t, saved, width-2), 2) + "\n\n")
	}
	discarded := ""
	if ed.Dirty() {
		discarded = countOf(max(len(lost), 1), "unsaved change", "unsaved changes") + " discarded"
	}
	switch {
	case saves > 0:
		b.WriteString(ui.Verdict(t, theme.StatusOK, "Saved "+countOf(len(saved), "change", "changes")+" to "+file.name, discarded))
		if file.normalized {
			b.WriteString("\n" + ui.Hint(t, "The file's comments and layout could not be kept and were rewritten in the standard form.", width))
		}
		if n := ed.SavedBlockers(); n > 0 {
			b.WriteString("\n" + ui.Hint(t, countOf(n, "blocker remains", "blockers remain")+" in the saved mapping; `taut preflight` lists them.", width))
		}
	case ed.Dirty():
		b.WriteString(ui.Verdict(t, theme.StatusWarn, "Quit without saving", discarded, file.name+" is untouched"))
	default:
		b.WriteString(ui.Verdict(t, theme.StatusOK, "No changes", v.Summary()))
	}
	b.WriteString("\n\n" + ui.Next(t, "taut preflight", "check this plan"))
	return b.String()
}

// --- writing the config file back --------------------------------------------------

// configFile is the config file on disk during a map session. It remembers
// the bytes that were read so a save can keep everything it did not change
// and can refuse to overwrite someone else's edit.
type configFile struct {
	path string
	// name is how the file is shown to the operator.
	name   string
	data   []byte
	loaded *config.Config
	// normalized is set when a save had to fall back to rewriting the
	// whole file in the standard form.
	normalized bool
}

// openConfigFile loads and validates the config file at path, keeping the
// bytes it was parsed from. The config file being edited and the baseline a
// save checks the file against must come from the same read: with two
// reads, an edit that lands between them would be in the baseline but not
// in the config file, and a save would quietly undo it.
func openConfigFile(path string) (*configFile, error) {
	m, data, err := config.LoadFile(path)
	if err != nil {
		return nil, err
	}
	if issues := m.Validate(); issues.HasErrors() {
		return nil, issues.Err()
	}
	return &configFile{path: path, name: filepath.Base(path), data: data, loaded: m}, nil
}

// editable reports, as an error, a file map can read but must not write
// back. A file holding more than one YAML document is the case: taut reads
// the first document only, and a save would write that one back and drop
// the rest.
func (f *configFile) editable() error {
	if !multiDocument(f.data) {
		return nil
	}
	return fail.New("map.multi_document", fmt.Sprintf("%s holds more than one YAML document, so map cannot save it", f.name),
		"taut reads the first document only; saving would write that one back and delete everything after the next `---` line.",
		fmt.Sprintf("Move everything after the second `---` line out of %s (or delete a trailing `---`), then run `taut map` again.", f.name)).
		WithExit(fail.ExitUsage)
}

// multiDocument reports whether data has anything after its first YAML
// document: another document, an empty one, or something that does not
// parse.
func multiDocument(data []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return false
	}
	return !errors.Is(dec.Decode(&doc), io.EOF)
}

// save writes m. A file in the form `taut init` produces is re-rendered in
// that form; a file someone has edited by hand is patched in place, touching
// only the channel selections that changed, so its comments and layout
// survive. Either way the result is parsed back and must equal m before it
// replaces the file.
//
// A config file reached through a symbolic link is written where the link
// points, so the link stays a link and its target is what changes.
func (f *configFile) save(m *config.Config) error {
	if err := f.editable(); err != nil {
		return err
	}
	changed := fail.New("map.config_changed", fmt.Sprintf("%s changed on disk while it was open in map", f.name),
		"Saving now would overwrite that change.",
		"Quit without saving, then run `taut map` again and repeat the edits.")
	// Resolved at every save: the guard below then reads the same file the
	// rename replaces, even if the link was pointed elsewhere meanwhile.
	target, err := filepath.EvalSymlinks(f.path)
	if err != nil {
		return changed
	}
	onDisk, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(onDisk, f.data) {
		return changed
	}

	standard, err := m.Marshal()
	if err != nil {
		return err
	}
	data, normalized := standard, false
	if canonical, err := f.loaded.Marshal(); err != nil || !bytes.Equal(canonical, f.data) {
		patched, ok := patchConfig(f.data, f.loaded, m)
		if ok && reads(patched, m) {
			data = patched
		} else {
			normalized = true
		}
	}
	// The full rewrite is held to the same standard as a patch: what is
	// about to replace the file must read back as the edited config file.
	if !reads(data, m) {
		return fail.New("map.save_unverified", fmt.Sprintf("%s was not saved", f.name),
			"The config file map was about to write did not read back as the edited mapping, so the file was left as it is.",
			fmt.Sprintf("Make the change by hand under target.teams[].channels in %s, then run `taut map` to check it. This is a bug in taut; please report it.", f.name))
	}
	if err := writeFileAtomic(target, data); err != nil {
		why := ""
		if errors.Is(err, fs.ErrPermission) {
			why = "the current user does not have permission to write there"
		}
		return fail.Wrap(err, "config.write_failed", fmt.Sprintf("The config file could not be written to %s", target), why,
			"Check that the file and its directory are writable, then save again.")
	}
	f.data, f.loaded = data, m
	f.normalized = f.normalized || normalized
	return nil
}

// reads reports whether data parses as exactly the config file m.
func reads(data []byte, m *config.Config) bool {
	back, err := config.Parse(data)
	return err == nil && back.Equal(m)
}

// writeFileAtomic replaces path with data by writing a temporary file beside
// it and renaming it into place. The file keeps its mode; a new file is 0600,
// as `taut init` writes it. path must not be a symbolic link (the rename
// would replace the link, not the file it points to); save resolves it.
func writeFileAtomic(path string, data []byte) error {
	mode := fs.FileMode(0o600)
	if st, err := os.Lstat(path); err == nil {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	// Make the rename itself durable. Best effort: not every platform or
	// file system can sync a directory, and the file is already in place.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// patchConfig rewrites only the channel selections that differ between
// before and after, inside the YAML document src, keeping every other node,
// and so every comment, as it was. It reports false when the document's shape
// is not one it can edit safely (aliases, merge keys, a team list that does
// not line up); the caller then falls back to a full rewrite.
func patchConfig(src []byte, before, after *config.Config) ([]byte, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, false
	}
	teams := yamlChild(yamlChild(doc.Content[0], "target"), "teams")
	if teams == nil || teams.Kind != yaml.SequenceNode ||
		len(teams.Content) != len(after.Target.Teams) || len(before.Target.Teams) != len(after.Target.Teams) {
		return nil, false
	}
	for i, team := range teams.Content {
		was, now := before.Target.Teams[i].Channels, after.Target.Teams[i].Channels
		sameLists := slices.Equal(was.Include, now.Include) && slices.Equal(was.Exclude, now.Exclude)
		if sameLists && maps.Equal(was.Rename, now.Rename) {
			continue
		}
		if team.Kind != yaml.MappingNode || yamlChild(team, "<<") != nil {
			return nil, false
		}
		channels := yamlChild(team, "channels")
		if channels == nil {
			channels = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			team.Content = append(team.Content, yamlString("channels"), channels)
		}
		if channels.Kind != yaml.MappingNode || yamlChild(channels, "<<") != nil {
			return nil, false
		}
		if !slices.Equal(was.Include, now.Include) && !yamlSetList(channels, "include", now.Include) {
			return nil, false
		}
		if !slices.Equal(was.Exclude, now.Exclude) && !yamlSetList(channels, "exclude", now.Exclude) {
			return nil, false
		}
		if !maps.Equal(was.Rename, now.Rename) && !yamlSetMap(channels, "rename", now.Rename) {
			return nil, false
		}
	}

	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil || enc.Close() != nil {
		return nil, false
	}
	// The encoder drops blank lines. Put one back before each commented
	// top-level section, which is where the standard form has them.
	var out bytes.Buffer
	prevBlank := true
	for _, line := range strings.SplitAfter(body.String(), "\n") {
		if strings.HasPrefix(line, "#") && !prevBlank {
			out.WriteByte('\n')
		}
		out.WriteString(line)
		prevBlank = strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#")
	}
	return out.Bytes(), true
}

func yamlString(v string) *yaml.Node {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
	}
	return &n
}

// yamlChild returns the value of key in a mapping node, or nil. Aliases are
// not followed: a document that uses them is left to the full rewrite.
func yamlChild(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func yamlRemove(n *yaml.Node, key string) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content = slices.Delete(n.Content, i, i+2)
			return
		}
	}
}

// yamlSetList makes key a list of values, reusing the existing item nodes so
// comments on entries that stay are kept.
func yamlSetList(parent *yaml.Node, key string, values []string) bool {
	if len(values) == 0 {
		yamlRemove(parent, key)
		return true
	}
	list := yamlChild(parent, key)
	if list == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		if len(values) <= 8 {
			list.Style = yaml.FlowStyle
		}
		parent.Content = append(parent.Content, yamlString(key), list)
	}
	if list.Kind != yaml.SequenceNode {
		return false
	}
	old := map[string]*yaml.Node{}
	for _, item := range list.Content {
		if item.Kind != yaml.ScalarNode {
			return false
		}
		old[item.Value] = item
	}
	list.Content = make([]*yaml.Node, len(values))
	for i, v := range values {
		if item, ok := old[v]; ok {
			list.Content[i] = item
			delete(old, v)
		} else {
			list.Content[i] = yamlString(v)
		}
	}
	// A long list reads better one entry per line.
	if len(values) > 8 {
		list.Style &^= yaml.FlowStyle
	}
	return true
}

// yamlSetMap makes key a mapping of values, keeping existing entries (and
// their comments) in place and adding new ones in sorted order.
func yamlSetMap(parent *yaml.Node, key string, values map[string]string) bool {
	if len(values) == 0 {
		yamlRemove(parent, key)
		return true
	}
	node := yamlChild(parent, key)
	if node == nil {
		node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		parent.Content = append(parent.Content, yamlString(key), node)
	}
	if node.Kind != yaml.MappingNode {
		return false
	}
	var kept []*yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		want, ok := values[k.Value]
		if !ok || seen[k.Value] || v.Kind != yaml.ScalarNode {
			continue
		}
		seen[k.Value] = true
		if v.Value != want {
			fresh := yamlString(want)
			fresh.LineComment = v.LineComment
			v = fresh
		}
		kept = append(kept, k, v)
	}
	var added []string
	for k := range values {
		if !seen[k] {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	for _, k := range added {
		kept = append(kept, yamlString(k), yamlString(values[k]))
	}
	node.Content = kept
	return true
}

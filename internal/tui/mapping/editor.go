// Package mapping is the engine and the full-screen view behind `taut map`:
// the Slack-to-Teams mapping as a tree that can be edited.
//
// The Editor holds a working copy of the config file and changes only the
// channel selection of its teams (include, exclude and rename). Where a
// channel goes is never decided here: after every edit the mapping is
// recomputed by preflight.Layout, the same function `taut preflight` reports and
// `taut migrate` executes, so the map cannot show something the migration
// would not do.
package mapping

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/identity"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/ui"
)

// ErrNoChange is returned by an edit that leaves the config file as it was, such
// as excluding a channel that is already not migrating.
var ErrNoChange = errors.New("no change")

// Change actions.
const (
	ActionExcluded = "excluded"
	ActionIncluded = "included"
	ActionMoved    = "moved"
	ActionRenamed  = "renamed"
)

// Change is one difference between two mappings, for one Slack channel.
type Change struct {
	SlackName string `json:"slackName"`
	// Action is one of the Action* constants.
	Action   string `json:"action"`
	FromTeam string `json:"fromTeam,omitempty"`
	ToTeam   string `json:"toTeam,omitempty"`
	// FromName and ToName are Teams channel names.
	FromName string `json:"fromName,omitempty"`
	ToName   string `json:"toName,omitempty"`
}

// Describe says what changed in a few words; arrow is the theme's "maps to"
// glyph.
func (c Change) Describe(arrow string) string {
	switch c.Action {
	case ActionExcluded:
		return "excluded from " + c.FromTeam
	case ActionIncluded:
		s := "included in " + c.ToTeam
		if c.ToName != c.SlackName {
			s += " as " + c.ToName
		}
		return s
	case ActionMoved:
		s := "moved from " + c.FromTeam + " to " + c.ToTeam
		if c.ToName != c.FromName {
			s += " as " + c.ToName
		}
		return s
	}
	return "renamed " + c.FromName + " " + arrow + " " + c.ToName
}

type snapshot struct {
	m      *config.Config
	layout *preflight.PlanLayout
	what   string
}

// Editor is an in-memory, undoable edit session over a config file's channel
// mapping.
type Editor struct {
	m        *config.Config
	channels []slack.Channel
	existing []string
	days     map[string]int
	byName   map[string]slack.Channel
	// byShown finds a channel by its name as the view shows it, for the
	// rare name that had to be sanitized to be drawn.
	byShown map[string]slack.Channel
	// dms are the export's direct messages and what becomes of each, set
	// by SetUsers. They do not depend on the edits: a direct message goes
	// into a chat, never into a team, and nothing here can move it.
	dms []preflight.DMPlan

	layout      *preflight.PlanLayout
	origLayout  *preflight.PlanLayout
	saved       *config.Config
	savedLayout *preflight.PlanLayout

	undo    []snapshot
	dirty   bool
	unsaved []Change
	view    *View
}

// NewEditor starts a session. The config file is copied; m itself is never
// modified. days maps a Slack channel ID to the number of days it has
// messages on, where known. existingTeams are team names already in the
// tenant; it may be nil.
func NewEditor(m *config.Config, channels []slack.Channel, existingTeams []string, days map[string]int) *Editor {
	e := &Editor{m: clone(m), channels: channels, existing: existingTeams, days: days, byName: map[string]slack.Channel{}, byShown: map[string]slack.Channel{}}
	for _, ch := range channels {
		if ch.Kind == slack.KindDM || ch.Kind == slack.KindMPIM || ch.Name == "" {
			continue
		}
		if _, dup := e.byName[ch.Name]; !dup {
			e.byName[ch.Name] = ch
		}
	}
	for _, ch := range channels {
		shown := ui.Sanitize(ch.Name)
		if ch.Kind == slack.KindDM || ch.Kind == slack.KindMPIM || shown == ch.Name || shown == "" {
			continue
		}
		if _, real := e.byName[shown]; real {
			continue
		}
		if _, dup := e.byShown[shown]; !dup {
			e.byShown[shown] = ch
		}
	}
	e.layout = preflight.Layout(e.m, channels, existingTeams)
	e.origLayout = e.layout
	e.saved, e.savedLayout = e.m, e.layout
	return e
}

// SetUsers gives the session the export's users, so the view can say what
// becomes of each direct message when the config file migrates them. The
// mapping view works offline: it knows what each participant is in Slack,
// not whether they have a Microsoft 365 account. `taut preflight` decides
// that, and may leave out a conversation shown here as migrating.
func (e *Editor) SetUsers(users []slack.User) {
	e.dms = preflight.DirectMessages(e.channels, users, identity.Resolve(users, nil, nil), false)
	sort.SliceStable(e.dms, func(i, j int) bool {
		a, b := e.dms[i], e.dms[j]
		if a.Migrates() != b.Migrates() {
			return a.Migrates()
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.SlackID < b.SlackID
	})
	e.view = nil
}

// Config returns the working copy. Callers must not modify it.
func (e *Editor) Config() *config.Config { return e.m }

// Layout returns the mapping the working copy implies.
func (e *Editor) Layout() *preflight.PlanLayout { return e.layout }

// Dirty reports whether the working copy differs from what was last saved.
func (e *Editor) Dirty() bool { return e.dirty }

// Unsaved lists what changed in the mapping since the last save.
func (e *Editor) Unsaved() []Change { return e.unsaved }

// Saved lists what changed in the mapping between the start of the session
// and the last save.
func (e *Editor) Saved() []Change { return diff(e.channels, e.origLayout, e.savedLayout) }

// SavedBlockers is the number of blockers in the mapping as last saved.
func (e *Editor) SavedBlockers() int { return len(e.savedLayout.Blockers) }

// MarkSaved records that the working copy has been written.
func (e *Editor) MarkSaved() {
	e.saved, e.savedLayout = e.m, e.layout
	e.refresh()
}

// CanUndo reports whether there is an edit to undo.
func (e *Editor) CanUndo() bool { return len(e.undo) > 0 }

// Undo reverts the last edit and returns its description.
func (e *Editor) Undo() (string, bool) {
	if len(e.undo) == 0 {
		return "", false
	}
	s := e.undo[len(e.undo)-1]
	e.undo = e.undo[:len(e.undo)-1]
	e.m, e.layout = s.m, s.layout
	e.refresh()
	return s.what, true
}

// TeamNames returns the config file's team names, in order.
func (e *Editor) TeamNames() []string {
	names := make([]string, len(e.m.Target.Teams))
	for i, t := range e.m.Target.Teams {
		names[i] = t.Name
	}
	return names
}

func (e *Editor) refresh() {
	e.dirty = !e.m.Equal(e.saved)
	e.unsaved = diff(e.channels, e.savedLayout, e.layout)
	e.view = nil
}

// apply runs one edit against a copy of the config file and adopts the copy only
// if the edit succeeds, the result is a valid config file, and verify (which
// sees the recomputed layout) accepts it.
func (e *Editor) apply(what string, mutate func(m *config.Config) error, verify func(l *preflight.PlanLayout) error) error {
	c := clone(e.m)
	if err := mutate(c); err != nil {
		return err
	}
	if c.Equal(e.m) {
		return ErrNoChange
	}
	if err := c.Validate().Err(); err != nil {
		return err
	}
	lay := preflight.Layout(c, e.channels, e.existing)
	if verify != nil {
		if err := verify(lay); err != nil {
			return err
		}
	}
	e.undo = append(e.undo, snapshot{m: e.m, layout: e.layout, what: what})
	e.m, e.layout = c, lay
	e.refresh()
	return nil
}

// Channel finds a channel of the export by name (with or without a leading
// "#") or by Slack ID.
func (e *Editor) Channel(ref string) (slack.Channel, error) {
	name := strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if ch, ok := e.byName[name]; ok {
		return ch, nil
	}
	for _, ch := range e.byName {
		if ch.ID == name {
			return ch, nil
		}
	}
	// The name as map displays it, when the export's own name could not
	// be drawn as it is.
	if ch, ok := e.byShown[name]; ok {
		return ch, nil
	}
	why := "The Slack export has no public or private channel with that name."
	var near []string
	for _, ch := range e.channels {
		if _, ok := e.byName[ch.Name]; !ok || name == "" {
			continue
		}
		if strings.EqualFold(ch.Name, name) || strings.Contains(strings.ToLower(ch.Name), strings.ToLower(name)) {
			near = append(near, "#"+ui.Sanitize(ch.Name))
		}
	}
	if len(near) > 0 {
		if len(near) > 3 {
			near = near[:3]
		}
		why += " Close matches: " + strings.Join(near, ", ") + "."
	}
	return slack.Channel{}, fail.New("map.unknown_channel", fmt.Sprintf("There is no channel named %q", name), why,
		"Run `taut map` to list every channel in the export, then use the Slack name exactly as shown.").WithExit(fail.ExitUsage)
}

func (e *Editor) team(m *config.Config, name string) (int, error) {
	want := strings.TrimSpace(name)
	for i, t := range m.Target.Teams {
		if strings.EqualFold(t.Name, want) {
			return i, nil
		}
	}
	quoted := make([]string, len(m.Target.Teams))
	for i, t := range m.Target.Teams {
		quoted[i] = fmt.Sprintf("%q", t.Name)
	}
	return 0, fail.New("map.unknown_team", fmt.Sprintf("There is no team named %q in the config file", want),
		"The config file defines: "+strings.Join(quoted, ", ")+".",
		"Use one of those names, or add the team under target.teams in the config file first.").WithExit(fail.ExitUsage)
}

// Exclude stops a channel from migrating.
func (e *Editor) Exclude(ref string) error {
	ch, err := e.Channel(ref)
	if err != nil {
		return err
	}
	return e.apply("Excluded #"+ui.Sanitize(ch.Name), func(m *config.Config) error {
		for _, i := range m.SelectingTeams(ch.Name, string(ch.Kind)) {
			deselect(&m.Target.Teams[i], ch)
		}
		return nil
	}, nil)
}

// Include makes a channel that is not migrating migrate into the named team.
// With an empty team name the team is inferred: the one that excludes the
// channel by name, or the only team there is. When it cannot be inferred the
// error has code "map.team_required".
func (e *Editor) Include(ref, teamName string) error {
	ch, err := e.Channel(ref)
	if err != nil {
		return err
	}
	if len(e.m.SelectingTeams(ch.Name, string(ch.Kind))) > 0 {
		if teamName == "" {
			return ErrNoChange
		}
		return e.Move(ref, teamName)
	}
	return e.apply("Included #"+ui.Sanitize(ch.Name), func(m *config.Config) error {
		var ti int
		switch {
		case teamName != "":
			if ti, err = e.team(m, teamName); err != nil {
				return err
			}
		case len(m.Target.Teams) == 1:
			ti = 0
		default:
			// The team that would take the channel again if it stopped
			// excluding it by name.
			var owners []int
			for i := range m.Target.Teams {
				t := m.Target.Teams[i]
				if at := indexOf(t.Channels.Exclude, ch.Name); at >= 0 {
					t.Channels.Exclude = without(t.Channels.Exclude, at)
					if t.Selects(ch.Name, string(ch.Kind)) {
						owners = append(owners, i)
					}
				}
			}
			if len(owners) != 1 {
				return fail.New("map.team_required", fmt.Sprintf("Which team should #%s go to?", ui.Sanitize(ch.Name)),
					fmt.Sprintf("The config file has %d teams and none of them is the obvious home for this channel.", len(m.Target.Teams)),
					fmt.Sprintf("Name the team: `taut map --move %s=<team name>`.", ui.Sanitize(ch.Name))).WithExit(fail.ExitUsage)
			}
			ti = owners[0]
		}
		return selectInto(m, ti, ch)
	}, nil)
}

// Move sends a channel to another team. A channel that is not migrating is
// included in that team.
func (e *Editor) Move(ref, teamName string) error {
	ch, err := e.Channel(ref)
	if err != nil {
		return err
	}
	if _, err := e.team(e.m, teamName); err != nil {
		return err
	}
	return e.apply("Moved #"+ui.Sanitize(ch.Name), func(m *config.Config) error {
		ti, err := e.team(m, teamName)
		if err != nil {
			return err
		}
		to := ""
		for _, i := range m.SelectingTeams(ch.Name, string(ch.Kind)) {
			if i == ti {
				continue
			}
			src := &m.Target.Teams[i]
			// The channel keeps the name it was given.
			if r := src.Channels.Rename[ch.Name]; r != "" && to == "" {
				to = r
			}
			delete(src.Channels.Rename, ch.Name)
			if len(src.Channels.Rename) == 0 {
				src.Channels.Rename = nil
			}
			deselect(src, ch)
		}
		if err := selectInto(m, ti, ch); err != nil {
			return err
		}
		dst := &m.Target.Teams[ti]
		if to != "" && dst.Channels.Rename[ch.Name] == "" {
			if dst.Channels.Rename == nil {
				dst.Channels.Rename = map[string]string{}
			}
			dst.Channels.Rename[ch.Name] = to
		}
		return nil
	}, nil)
}

// Destinations lists the teams a channel could go to, other than the one it
// is in. When there are none it returns why the first candidate refused.
func (e *Editor) Destinations(ref string) ([]string, error) {
	ch, err := e.Channel(ref)
	if err != nil {
		return nil, err
	}
	sel := e.m.SelectingTeams(ch.Name, string(ch.Kind))
	var teams []string
	var refusal error
	for i, t := range e.m.Target.Teams {
		if len(sel) == 1 && sel[0] == i {
			continue
		}
		if err := selectInto(clone(e.m), i, ch); err != nil {
			if refusal == nil {
				refusal = err
			}
			continue
		}
		teams = append(teams, t.Name)
	}
	if len(teams) > 0 {
		return teams, nil
	}
	return nil, refusal
}

// CheckName reports whether Teams accepts name as a channel name, using the
// rules preflight applies to every channel. The error says why not and what
// Teams would accept instead.
func CheckName(name string) error {
	if accepted, reason := preflight.TeamsChannelName(name); accepted != name {
		return fail.New("map.invalid_name", fmt.Sprintf("Teams does not accept %q as a channel name", name),
			"The name "+reason+".", fmt.Sprintf("Use %q, or another name Teams allows.", accepted)).WithExit(fail.ExitUsage)
	}
	return nil
}

// Rename sets the Teams name of a migrating channel. An empty name, or the
// Slack name itself, removes the rename.
func (e *Editor) Rename(ref, to string) error {
	ch, err := e.Channel(ref)
	if err != nil {
		return err
	}
	to = strings.TrimSpace(to)
	sel := e.m.SelectingTeams(ch.Name, string(ch.Kind))
	switch {
	case len(sel) == 0:
		return fail.New("map.not_migrating", fmt.Sprintf("#%s is not migrating, so it cannot be renamed", ui.Sanitize(ch.Name)), "",
			fmt.Sprintf("Include it first: `taut map --include %s`.", ui.Sanitize(ch.Name))).WithExit(fail.ExitUsage)
	case ch.IsGeneral:
		return fail.New("map.general", fmt.Sprintf("#%s cannot be renamed", ui.Sanitize(ch.Name)),
			"It is the workspace's general channel, which always becomes the team's built-in General channel.",
			"Leave it as it is, or exclude it.").WithExit(fail.ExitUsage)
	}
	if to == ch.Name {
		to = ""
	}
	if to != "" {
		if err := CheckName(to); err != nil {
			return err
		}
	}
	ti := sel[0]
	return e.apply("Renamed #"+ui.Sanitize(ch.Name), func(m *config.Config) error {
		c := &m.Target.Teams[ti].Channels
		if to == "" {
			delete(c.Rename, ch.Name)
			if len(c.Rename) == 0 {
				c.Rename = nil
			}
			return nil
		}
		if c.Rename == nil {
			c.Rename = map[string]string{}
		}
		c.Rename[ch.Name] = to
		return nil
	}, func(l *preflight.PlanLayout) error {
		if to == "" {
			return nil
		}
		// Layout has the last word. If it had to alter the name, or gave
		// it to this channel by taking it from another, the rename is not
		// what was asked for.
		was, is := places(e.layout), places(l)
		if got := is[ch.Name]; got.name != to {
			reason := "another channel already maps to this name"
			for _, cp := range l.Teams[ti].Channels {
				if cp.SlackName == ch.Name && cp.RenameReason != "" {
					reason = cp.RenameReason
				}
			}
			return nameTaken(ui.Sanitize(ch.Name), to, l.Teams[ti].Name, reason)
		}
		for name, p := range is {
			if name != ch.Name && was[name] != p {
				return nameTaken(ui.Sanitize(ch.Name), to, l.Teams[ti].Name, fmt.Sprintf("#%s already maps to it", ui.Sanitize(name)))
			}
		}
		return nil
	})
}

func nameTaken(channel, to, team, reason string) error {
	return fail.New("map.name_taken", fmt.Sprintf("#%s cannot be named %q in %s", channel, to, team),
		"The name is unavailable: "+reason+".",
		"Choose a name no other channel in the team maps to.").WithExit(fail.ExitUsage)
}

// --- selection edits ------------------------------------------------------------

// deselect makes t stop selecting ch, with the smallest change that does:
// dropping the include entry that names it, else adding an exclude entry.
func deselect(t *config.TeamSpec, ch slack.Channel) {
	kind := string(ch.Kind)
	c := &t.Channels
	if at := indexOf(c.Include, ch.Name); at >= 0 && len(c.Include) > 1 {
		c.Include = without(c.Include, at)
	}
	if t.Selects(ch.Name, kind) {
		c.Exclude = append(c.Exclude, literal(ch.Name))
	}
}

// selectInto makes team ti select ch, with the smallest change that does:
// dropping the exclude entry that names it, then adding an include entry. It
// refuses when only a wider change to the config file could do it.
func selectInto(m *config.Config, ti int, ch slack.Channel) error {
	t := &m.Target.Teams[ti]
	kind := string(ch.Kind)
	if t.Selects(ch.Name, kind) {
		return nil
	}
	field := fmt.Sprintf("target.teams[%d].channels", ti)

	trial := *t
	trial.Channels.Exclude = nil
	trial.Channels.Include = nil
	if !trial.Selects(ch.Name, kind) {
		return fail.New("map.kind_not_selected",
			fmt.Sprintf("#%s is a %s channel, and %s takes no %s channels", ui.Sanitize(ch.Name), kind, t.Name, kind),
			fmt.Sprintf("Channel kinds are chosen per team (%s.types), not per channel, so one %s channel cannot be added on its own.", field, kind),
			fmt.Sprintf("Add %s to %s.types in the config file, then exclude the %s channels you do not want.", kind, field, kind)).WithExit(fail.ExitUsage)
	}

	c := &t.Channels
	for at := indexOf(c.Exclude, ch.Name); at >= 0; at = indexOf(c.Exclude, ch.Name) {
		c.Exclude = without(c.Exclude, at)
	}
	if len(c.Exclude) == 0 {
		c.Exclude = nil
	}
	for _, p := range c.Exclude {
		if ok, err := path.Match(p, ch.Name); err == nil && ok {
			return fail.New("map.excluded_by_pattern",
				fmt.Sprintf("#%s is excluded from %s by the pattern %q", ui.Sanitize(ch.Name), t.Name, p),
				"Exclude patterns win over includes, and this one covers other channels too, so it cannot be lifted for one channel.",
				fmt.Sprintf("Narrow or remove %q under %s.exclude in the config file.", p, field)).WithExit(fail.ExitUsage)
		}
	}
	if !t.Selects(ch.Name, kind) {
		c.Include = append(c.Include, literal(ch.Name))
	}
	return nil
}

// literal returns the glob pattern that matches exactly name.
func literal(name string) string {
	if !strings.ContainsAny(name, `*?[\`) {
		return name
	}
	var b strings.Builder
	for _, r := range name {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// indexOf finds the pattern in list that names exactly the channel.
func indexOf(list []string, name string) int {
	lit := literal(name)
	for i, p := range list {
		if p == lit {
			return i
		}
	}
	return -1
}

func without(list []string, at int) []string {
	out := slices.Delete(slices.Clone(list), at, at+1)
	if len(out) == 0 {
		return nil
	}
	return out
}

// clone copies a config file deeply enough that editing the copy's channel
// selections cannot touch the original.
func clone(m *config.Config) *config.Config {
	c := *m
	c.Target.Teams = make([]config.TeamSpec, len(m.Target.Teams))
	for i, t := range m.Target.Teams {
		t.Channels.Include = slices.Clone(t.Channels.Include)
		t.Channels.Exclude = slices.Clone(t.Channels.Exclude)
		t.Channels.Types = slices.Clone(t.Channels.Types)
		t.Channels.Rename = maps.Clone(t.Channels.Rename)
		c.Target.Teams[i] = t
	}
	return &c
}

// --- diff -------------------------------------------------------------------------

type place struct{ team, name string }

func places(l *preflight.PlanLayout) map[string]place {
	out := map[string]place{}
	for _, t := range l.Teams {
		for _, c := range t.Channels {
			if _, dup := out[c.SlackName]; !dup {
				out[c.SlackName] = place{t.Name, c.TargetName}
			}
		}
	}
	return out
}

// diff compares two layouts channel by channel, in export order. Because it
// compares what Layout computed, it also reports side effects, such as a
// channel whose "-2" suffix went away when its namesake was excluded.
func diff(channels []slack.Channel, before, after *preflight.PlanLayout) []Change {
	if before == after {
		return nil
	}
	was, is := places(before), places(after)
	var out []Change
	seen := map[string]bool{}
	for _, ch := range channels {
		if ch.Name == "" || seen[ch.Name] {
			continue
		}
		seen[ch.Name] = true
		a, wasIn := was[ch.Name]
		b, isIn := is[ch.Name]
		// Changes are shown to the operator, so they carry names as map
		// displays them (see View).
		c := Change{
			SlackName: ui.Sanitize(ch.Name), FromTeam: ui.Sanitize(a.team), ToTeam: ui.Sanitize(b.team),
			FromName: ui.Sanitize(a.name), ToName: ui.Sanitize(b.name),
		}
		switch {
		case wasIn && !isIn:
			c.Action = ActionExcluded
		case !wasIn && isIn:
			c.Action = ActionIncluded
		case !wasIn:
			continue
		case a.team != b.team:
			c.Action = ActionMoved
		case a.name != b.name:
			c.Action = ActionRenamed
		default:
			continue
		}
		out = append(out, c)
	}
	return out
}

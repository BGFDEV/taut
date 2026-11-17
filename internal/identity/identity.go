// Package identity maps Slack users to Microsoft Entra users.
//
// Resolution order is: the override CSV, then email. Anyone left over is
// unmapped, with a reason; what happens to their messages is decided by the
// config file's identity policy.
package identity

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/slack"
)

// DirUser is an Entra user as far as mapping is concerned.
type DirUser struct {
	ID          string
	DisplayName string
	Mail        string
	UPN         string
	Enabled     bool
	// Guest is true for a guest account of the tenant (userType "Guest"):
	// somebody from outside, invited in.
	Guest bool
	// Aliases are additional SMTP addresses (proxyAddresses).
	Aliases []string
}

// Directory is an in-memory index of Entra users by ID and by every address
// they answer to. Lookups are case-insensitive: Slack preserves the case a
// user typed, Entra does not.
type Directory struct {
	users []DirUser
	index map[string]int
	// ambiguous holds the keys that more than one user answers to. Find
	// returns the first of them, which is a guess.
	ambiguous map[string]bool
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// NewDirectory indexes users.
func NewDirectory(users []DirUser) *Directory {
	d := &Directory{users: users, index: make(map[string]int, len(users)*3), ambiguous: map[string]bool{}}
	add := func(key string, i int) {
		if key = norm(key); key != "" {
			switch first, taken := d.index[key]; {
			case !taken:
				d.index[key] = i
			case first != i:
				// Two accounts answer to this key: one's mail is the
				// other's UPN, say. The first keeps it, and the key is
				// remembered as one that does not identify anybody.
				d.ambiguous[key] = true
			}
		}
	}
	// Primary identifiers first so an alias can never shadow another
	// user's own address.
	for i, u := range users {
		add(u.ID, i)
		add(u.UPN, i)
		add(u.Mail, i)
	}
	for i, u := range users {
		for _, a := range u.Aliases {
			if rest, ok := strings.CutPrefix(norm(a), "smtp:"); ok {
				add(rest, i)
			}
		}
	}
	return d
}

// Len returns the number of users.
func (d *Directory) Len() int { return len(d.users) }

// Find looks a user up by object ID, UPN, mail or alias.
func (d *Directory) Find(key string) (DirUser, bool) {
	if d == nil {
		return DirUser{}, false
	}
	i, ok := d.index[norm(key)]
	if !ok {
		return DirUser{}, false
	}
	return d.users[i], true
}

// Ambiguous reports whether more than one user of the directory answers to
// key. Find still returns one of them, but which is an accident of order;
// nothing that gives a person access should rest on such a match.
func (d *Directory) Ambiguous(key string) bool {
	return d != nil && d.ambiguous[norm(key)]
}

// Override is one row of the override CSV.
type Override struct {
	SlackID    string
	SlackEmail string
	// Entra is the target's UPN, mail or object ID.
	Entra string
	Line  int
}

// LoadOverrides reads the override CSV. It needs a header row naming the
// columns; "entra" is required, plus at least one of "slack_user_id" and
// "slack_email". Other columns (a "note", say) are ignored.
func LoadOverrides(path string) ([]Override, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fail.Wrap(err, "identity.overrides_unreadable",
			"The identity override file could not be read", "",
			"Check identity.overrides in the config file; the path is relative to the config file.")
	}
	defer f.Close()
	return ParseOverrides(f)
}

// ParseOverrides reads override CSV data.
func ParseOverrides(r io.Reader) ([]Override, error) {
	const example = "slack_user_id,slack_email,entra\nU024BE7LH,,dana@acme.com"
	bad := func(why string) error {
		return fail.New("identity.overrides_invalid", "The identity override file is not valid", why,
			"Use a CSV with a header row, for example:\n"+example)
	}

	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, bad(err.Error())
	}
	col := map[string]int{}
	for i, h := range header {
		col[norm(strings.TrimPrefix(h, "\uFEFF"))] = i
	}
	iEntra, okEntra := col["entra"]
	iID, okID := col["slack_user_id"]
	iEmail, okEmail := col["slack_email"]
	if !okEntra || (!okID && !okEmail) {
		return nil, bad(`the header must name an "entra" column and "slack_user_id" and/or "slack_email"`)
	}

	get := func(rec []string, i int, ok bool) string {
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	var out []Override
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line, _ := cr.FieldPos(0)
		if err != nil {
			return nil, bad(err.Error())
		}
		o := Override{SlackID: get(rec, iID, okID), SlackEmail: get(rec, iEmail, okEmail), Entra: get(rec, iEntra, okEntra), Line: line}
		if o.SlackID == "" && o.SlackEmail == "" && o.Entra == "" {
			continue // blank row
		}
		if o.Entra == "" || (o.SlackID == "" && o.SlackEmail == "") {
			return nil, bad(fmt.Sprintf("line %d needs an entra value and a slack_user_id or slack_email", line))
		}
		out = append(out, o)
	}
	return out, nil
}

// Method is how a mapping was made.
type Method string

// Mapping methods.
const (
	MethodEmail    Method = "email"
	MethodOverride Method = "override"
	MethodNone     Method = "unmapped"
)

// Reason says why a user is unmapped.
type Reason string

// Unmapped reasons.
const (
	ReasonBot           Reason = "bot"
	ReasonDeactivated   Reason = "deactivated"
	ReasonExternal      Reason = "external"
	ReasonNoEmail       Reason = "no_email"
	ReasonNoMatch       Reason = "no_match"
	ReasonOverrideStale Reason = "override_target_missing"
	ReasonNotInExport   Reason = "not_in_export"
)

// Describe returns a short human phrase for the reason.
func (r Reason) Describe() string {
	switch r {
	case ReasonBot:
		return "bot or app"
	case ReasonDeactivated:
		return "deactivated in Slack"
	case ReasonExternal:
		return "external or guest"
	case ReasonNoEmail:
		return "no email in the export"
	case ReasonNoMatch:
		return "email not in Entra"
	case ReasonOverrideStale:
		return "override target not in Entra"
	case ReasonNotInExport:
		return "not in the export's user list"
	}
	return string(r)
}

// Entry is the mapping decision for one Slack user.
type Entry struct {
	SlackID string `json:"slackId"`
	Name    string `json:"name"`
	Email   string `json:"email,omitempty"`
	Method  Method `json:"method"`
	// EntraID and EntraUPN are set when mapped.
	EntraID  string `json:"entraId,omitempty"`
	EntraUPN string `json:"entraUpn,omitempty"`
	// TargetDisabled is true when the matched Entra account is disabled.
	TargetDisabled bool `json:"targetDisabled,omitempty"`
	// TargetGuest is true when the matched Entra account is a guest
	// account, and Ambiguous when the address it was matched by belongs
	// to more than one account.
	TargetGuest bool   `json:"targetGuest,omitempty"`
	Ambiguous   bool   `json:"ambiguous,omitempty"`
	Reason      Reason `json:"reason,omitempty"`
}

// Mapped reports whether the user resolved to an Entra user.
func (e *Entry) Mapped() bool { return e != nil && e.Method != MethodNone }

// Map is the result of resolving every Slack user.
type Map struct {
	entries map[string]*Entry
	order   []string
}

// Resolve maps users against the directory. A nil directory maps nobody by
// email, which is how an offline preflight behaves.
func Resolve(users []slack.User, dir *Directory, overrides []Override) *Map {
	byID := map[string]Override{}
	byEmail := map[string]Override{}
	for _, o := range overrides {
		if o.SlackID != "" {
			byID[o.SlackID] = o
		}
		if o.SlackEmail != "" {
			byEmail[norm(o.SlackEmail)] = o
		}
	}

	m := &Map{entries: make(map[string]*Entry, len(users))}
	for _, u := range users {
		e := &Entry{SlackID: u.ID, Name: u.DisplayLabel(), Email: strings.TrimSpace(u.Profile.Email), Method: MethodNone}

		ov, has := byID[u.ID]
		if !has && e.Email != "" {
			ov, has = byEmail[norm(e.Email)]
		}
		switch {
		case has:
			if du, ok := dir.Find(ov.Entra); ok {
				e.set(MethodOverride, du)
				e.Ambiguous = dir.Ambiguous(ov.Entra)
			} else {
				e.Reason = ReasonOverrideStale
			}
		case e.Email != "":
			if du, ok := dir.Find(e.Email); ok {
				e.set(MethodEmail, du)
				e.Ambiguous = dir.Ambiguous(e.Email)
			}
		}
		if e.Method == MethodNone && e.Reason == "" {
			e.Reason = unmappedReason(u, e.Email)
		}
		m.entries[u.ID] = e
		m.order = append(m.order, u.ID)
	}
	return m
}

func (e *Entry) set(method Method, du DirUser) {
	e.Method, e.EntraID, e.EntraUPN, e.TargetDisabled, e.TargetGuest = method, du.ID, du.UPN, !du.Enabled, du.Guest
}

func unmappedReason(u slack.User, email string) Reason {
	switch {
	case u.IsBot || u.IsAppUser || u.ID == "USLACKBOT":
		return ReasonBot
	case u.IsStranger || u.IsGuest():
		return ReasonExternal
	case u.Deleted:
		return ReasonDeactivated
	case email == "":
		return ReasonNoEmail
	}
	return ReasonNoMatch
}

// Lookup returns the entry for a Slack user ID, or nil if the ID was never
// seen.
func (m *Map) Lookup(slackID string) *Entry { return m.entries[slackID] }

// AddUnknown records an author that appears in messages but not in the
// export's user list, and returns its entry. Calling it again for the same
// ID returns the existing entry.
func (m *Map) AddUnknown(slackID, name string) *Entry {
	if e, ok := m.entries[slackID]; ok {
		return e
	}
	reason := ReasonNotInExport
	if slackID == "USLACKBOT" || strings.HasPrefix(slackID, "B") {
		reason = ReasonBot
	}
	if name == "" {
		name = slackID
	}
	e := &Entry{SlackID: slackID, Name: name, Method: MethodNone, Reason: reason}
	m.entries[slackID] = e
	m.order = append(m.order, slackID)
	return e
}

// Entries returns every entry in first-seen order.
func (m *Map) Entries() []*Entry {
	out := make([]*Entry, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.entries[id])
	}
	return out
}

// Counts tallies entries by method.
func (m *Map) Counts() (byEmail, byOverride, unmapped int) {
	for _, e := range m.entries {
		switch e.Method {
		case MethodEmail:
			byEmail++
		case MethodOverride:
			byOverride++
		default:
			unmapped++
		}
	}
	return
}

// Unmapped returns unmapped entries sorted by name.
func (m *Map) Unmapped() []*Entry {
	var out []*Entry
	for _, e := range m.entries {
		if !e.Mapped() {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].SlackID < out[j].SlackID
	})
	return out
}

// Package identity maps Slack users to Microsoft Entra users.
//
// Resolution order is: the override CSV, then the email address, then, if
// the config file turns them on, the part of the email address before the
// "@" and the display name. Anyone left over is unmapped, with a reason;
// what happens to their messages is decided by the config file's identity
// policy.
//
// The last two matchers guess. They match a Slack user only to an account
// nobody was matched to by a stronger method, only when exactly one account
// answers to the name, and only when no other Slack user would get the same
// account. Every such match is marked, so `taut preflight` can list it for
// review.
package identity

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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
	// None is set by the entra value "none": the Slack user has no account
	// and must not be matched to one by any matcher.
	None bool
	Line int
}

// OverrideNone is the entra value of an override row that says a Slack user
// has no Entra account. It is how an operator rejects a match by email
// local part or display name that turned out to be someone else.
const OverrideNone = "none"

// Options selects the matchers Resolve uses after the override CSV and the
// exact email address. Both are off by default.
type Options struct {
	// EmailLocalPart matches the part of the Slack email address before
	// the "@" against the same part of the Entra user's UPN, mail and
	// aliases.
	EmailLocalPart bool
	// DisplayName matches the Slack real name and display name against the
	// Entra display name, ignoring case and extra spaces.
	DisplayName bool
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
		if norm(o.Entra) == OverrideNone {
			o.Entra, o.None = "", true
		}
		if o.SlackID == "" && o.SlackEmail == "" && o.Entra == "" {
			continue // blank row
		}
		if (o.Entra == "" && !o.None) || (o.SlackID == "" && o.SlackEmail == "") {
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
	// MethodEmailLocalPart and MethodDisplayName are the opt-in matchers.
	MethodEmailLocalPart Method = "email_local_part"
	MethodDisplayName    Method = "display_name"
	MethodNone           Method = "unmapped"
)

// Weak reports whether the method is one of the matchers that guess: the
// email local part or the display name.
func (m Method) Weak() bool { return m == MethodEmailLocalPart || m == MethodDisplayName }

// Describe returns a short human phrase for the method.
func (m Method) Describe() string {
	switch m {
	case MethodEmail:
		return "email address"
	case MethodOverride:
		return "override CSV"
	case MethodEmailLocalPart:
		return "name part of the email address"
	case MethodDisplayName:
		return "display name"
	case MethodNone:
		return "not matched"
	}
	return string(m)
}

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
	// ReasonExcluded: the override CSV says the user has no account.
	ReasonExcluded Reason = "override_none"
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
	case ReasonExcluded:
		return "marked as having no account in the override CSV"
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
	// MatchedOn is what a weak matcher matched: the email local part or
	// the display name the Slack user and the Entra user share.
	MatchedOn string `json:"matchedOn,omitempty"`
	// Undecided says why a weak matcher that found candidates matched
	// nobody: UndecidedSeveralAccounts or UndecidedSeveralUsers.
	Undecided string `json:"undecided,omitempty"`
	// Excluded is true when the override CSV says the user has no account.
	Excluded bool `json:"excluded,omitempty"`
}

// Why a weak matcher left a user unmatched although a name fitted.
const (
	// UndecidedSeveralAccounts: more than one Entra account answers to the
	// user's email local part or display name.
	UndecidedSeveralAccounts = "several_accounts"
	// UndecidedSeveralUsers: more than one Slack user would be matched to
	// the same Entra account.
	UndecidedSeveralUsers = "several_slack_users"
)

// Mapped reports whether the user resolved to an Entra user.
func (e *Entry) Mapped() bool { return e != nil && e.Method != MethodNone }

// Map is the result of resolving every Slack user.
type Map struct {
	entries map[string]*Entry
	order   []string
}

// Resolve maps users against the directory. A nil directory maps nobody,
// which is how an offline preflight behaves.
func Resolve(users []slack.User, dir *Directory, overrides []Override, opts Options) *Map {
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
		case has && ov.None:
			// What the user is in Slack still says most about why they
			// have no account; say "excluded" only when nothing else does.
			e.Excluded = true
			if e.Reason = unmappedReason(u, e.Email); e.Reason == ReasonNoMatch || e.Reason == ReasonNoEmail {
				e.Reason = ReasonExcluded
			}
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
	if dir != nil {
		if opts.EmailLocalPart {
			m.matchWeakly(users, dir, MethodEmailLocalPart)
		}
		if opts.DisplayName {
			m.matchWeakly(users, dir, MethodDisplayName)
		}
	}
	return m
}

// localPart returns the part of an address before its "@", normalized, or
// "" if the address has no such part.
func localPart(addr string) string {
	addr = norm(addr)
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 {
		return ""
	}
	return addr[:at]
}

// normName normalizes a display name for comparison: lower case, with runs
// of white space reduced to one space.
func normName(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

// weakKeys returns what a matcher compares for a Slack user and for an
// Entra user: the local parts of their addresses, or their names.
func weakKeys(method Method, u *slack.User, du *DirUser) []string {
	var raw []string
	switch {
	case method == MethodEmailLocalPart && u != nil:
		raw = []string{localPart(u.Profile.Email)}
	case method == MethodEmailLocalPart:
		raw = []string{localPart(du.UPN), localPart(du.Mail)}
		for _, a := range du.Aliases {
			if rest, ok := strings.CutPrefix(norm(a), "smtp:"); ok {
				raw = append(raw, localPart(rest))
			}
		}
	case u != nil:
		raw = []string{normName(u.Profile.RealName), normName(u.RealName), normName(u.Profile.DisplayName)}
	default:
		raw = []string{normName(du.DisplayName)}
	}
	var out []string
	for _, k := range raw {
		if k != "" && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// weakCandidate reports whether a Slack user may be matched by a matcher
// that guesses. Bots and apps have names, not identities. Guests and Slack
// Connect users belong to other organisations, where the same name is
// somebody else.
func weakCandidate(u slack.User) bool {
	return !u.IsBot && !u.IsAppUser && u.ID != "USLACKBOT" && !u.IsStranger && !u.IsGuest()
}

// matchWeakly runs one of the matchers that guess over the users nothing
// stronger has matched.
//
// An Entra account is a candidate only if it is a member account (not a
// guest of the tenant) that no Slack user is matched to yet. A Slack user is
// matched when exactly one candidate answers to their key, and nobody else
// among the unmatched Slack users would be matched to that same account.
// Anything else leaves the user unmatched, with Undecided saying which rule
// stopped it.
func (m *Map) matchWeakly(users []slack.User, dir *Directory, method Method) {
	taken := map[string]bool{}
	for _, e := range m.entries {
		if e.Mapped() {
			taken[norm(e.EntraID)] = true
		}
	}
	// key → the candidate accounts that answer to it.
	byKey := map[string][]int{}
	for i := range dir.users {
		du := &dir.users[i]
		if du.Guest || taken[norm(du.ID)] {
			continue
		}
		for _, k := range weakKeys(method, nil, du) {
			byKey[k] = append(byKey[k], i)
		}
	}

	type proposal struct {
		entry *Entry
		key   string
	}
	proposed := map[int][]proposal{} // account → the Slack users who would get it
	var accounts []int
	for i := range users {
		u := &users[i]
		e := m.entries[u.ID]
		if e == nil || e.Mapped() || e.Excluded || e.Reason == ReasonOverrideStale || !weakCandidate(*u) {
			continue
		}
		// Every key of the user must point at the same single account. A
		// real name that fits one account and a display name that fits
		// another is not a match.
		found, key, several := -1, "", false
		for _, k := range weakKeys(method, u, nil) {
			for _, c := range byKey[k] {
				if found >= 0 && found != c {
					several = true
				}
				if found < 0 {
					found, key = c, k
				}
			}
		}
		switch {
		case several:
			e.Undecided = UndecidedSeveralAccounts
		case found >= 0:
			if len(proposed[found]) == 0 {
				accounts = append(accounts, found)
			}
			proposed[found] = append(proposed[found], proposal{e, key})
		}
	}
	for _, i := range accounts {
		ps := proposed[i]
		if len(ps) > 1 {
			for _, p := range ps {
				p.entry.Undecided = UndecidedSeveralUsers
			}
			continue
		}
		e := ps[0].entry
		e.set(method, dir.users[i])
		e.MatchedOn, e.Reason, e.Undecided = ps[0].key, "", ""
	}
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

// Counts is the number of entries by method.
type Counts struct {
	Email, Override, EmailLocalPart, DisplayName, Unmapped int
}

// Total is the number of entries.
func (c Counts) Total() int {
	return c.Email + c.Override + c.EmailLocalPart + c.DisplayName + c.Unmapped
}

// Counts tallies entries by method.
func (m *Map) Counts() Counts {
	var c Counts
	for _, e := range m.entries {
		switch e.Method {
		case MethodEmail:
			c.Email++
		case MethodOverride:
			c.Override++
		case MethodEmailLocalPart:
			c.EmailLocalPart++
		case MethodDisplayName:
			c.DisplayName++
		default:
			c.Unmapped++
		}
	}
	return c
}

// Weak returns the entries matched by a matcher that guesses, sorted by
// name: the ones an operator should review.
func (m *Map) Weak() []*Entry {
	var out []*Entry
	for _, e := range m.entries {
		if e.Method.Weak() {
			out = append(out, e)
		}
	}
	sortEntries(out)
	return out
}

func sortEntries(out []*Entry) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].SlackID < out[j].SlackID
	})
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

package fixtures

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	teamID      = "T0FIXTURE"
	workspace   = "taut-fixture"
	emailDomain = "fixture.example"
	partnerMail = "partner.example"
	slackbotID  = "USLACKBOT"
)

var (
	// baseTime is when every fixture's history starts: a Monday morning.
	baseTime = time.Date(2024, 3, 4, 9, 0, 0, 0, time.UTC)
	// zipTime is the modification time stamped on every archive entry.
	zipTime = time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC)
)

// M is a JSON object. encoding/json writes map keys sorted, which keeps the
// output deterministic.
type M = map[string]any

// stamp is a Slack timestamp as microseconds since the Unix epoch.
type stamp int64

func at(t time.Time) stamp { return stamp(t.UnixMicro()) }

func (s stamp) String() string { return fmt.Sprintf("%d.%06d", int64(s)/1e6, int64(s)%1e6) }
func (s stamp) sec() int64     { return int64(s) / 1e6 }
func (s stamp) ms() int64      { return int64(s) / 1e3 }
func (s stamp) day() string    { return time.UnixMicro(int64(s)).UTC().Format("2006-01-02") }

func (s stamp) add(d time.Duration) stamp { return s + stamp(d.Microseconds()) }

type user struct {
	ID                string  `json:"id"`
	TeamID            string  `json:"team_id"`
	Name              string  `json:"name"`
	Deleted           bool    `json:"deleted"`
	RealName          string  `json:"real_name,omitempty"`
	Profile           profile `json:"profile"`
	IsBot             bool    `json:"is_bot"`
	IsAppUser         bool    `json:"is_app_user"`
	IsRestricted      bool    `json:"is_restricted,omitempty"`
	IsUltraRestricted bool    `json:"is_ultra_restricted,omitempty"`
	IsStranger        bool    `json:"is_stranger,omitempty"`
	Updated           int64   `json:"updated"`
}

type profile struct {
	RealName    string `json:"real_name"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email,omitempty"`
	BotID       string `json:"bot_id,omitempty"`
	Team        string `json:"team"`
}

type msgProfile struct {
	DisplayName string `json:"display_name"`
	RealName    string `json:"real_name"`
	Name        string `json:"name,omitempty"`
	Team        string `json:"team,omitempty"`
}

type replyRef struct {
	User string `json:"user"`
	TS   string `json:"ts"`
}

type edited struct {
	User string `json:"user"`
	TS   string `json:"ts"`
}

type reaction struct {
	Name  string   `json:"name"`
	Users []string `json:"users"`
	Count int      `json:"count"`
}

type file struct {
	ID                 string `json:"id"`
	Created            int64  `json:"created,omitempty"`
	Timestamp          int64  `json:"timestamp,omitempty"`
	Name               string `json:"name,omitempty"`
	Title              string `json:"title,omitempty"`
	Mimetype           string `json:"mimetype,omitempty"`
	Filetype           string `json:"filetype,omitempty"`
	PrettyType         string `json:"pretty_type,omitempty"`
	User               string `json:"user,omitempty"`
	UserTeam           string `json:"user_team,omitempty"`
	Size               int64  `json:"size,omitempty"`
	Mode               string `json:"mode,omitempty"`
	IsExternal         bool   `json:"is_external,omitempty"`
	ExternalType       string `json:"external_type,omitempty"`
	ExternalURL        string `json:"external_url,omitempty"`
	URLPrivate         string `json:"url_private,omitempty"`
	URLPrivateDownload string `json:"url_private_download,omitempty"`
	Permalink          string `json:"permalink,omitempty"`
	HiddenByLimit      bool   `json:"hidden_by_limit,omitempty"`
}

type message struct {
	Type            string      `json:"type,omitempty"`
	Subtype         string      `json:"subtype,omitempty"`
	User            string      `json:"user,omitempty"`
	BotID           string      `json:"bot_id,omitempty"`
	Username        string      `json:"username,omitempty"`
	Text            string      `json:"text"`
	TS              string      `json:"ts"`
	Team            string      `json:"team,omitempty"`
	UserTeam        string      `json:"user_team,omitempty"`
	SourceTeam      string      `json:"source_team,omitempty"`
	UserProfile     *msgProfile `json:"user_profile,omitempty"`
	BotProfile      M           `json:"bot_profile,omitempty"`
	ThreadTS        string      `json:"thread_ts,omitempty"`
	ParentUserID    string      `json:"parent_user_id,omitempty"`
	ReplyCount      int         `json:"reply_count,omitempty"`
	ReplyUsersCount int         `json:"reply_users_count,omitempty"`
	LatestReply     string      `json:"latest_reply,omitempty"`
	ReplyUsers      []string    `json:"reply_users,omitempty"`
	Replies         []replyRef  `json:"replies,omitempty"`
	Edited          *edited     `json:"edited,omitempty"`
	Reactions       []reaction  `json:"reactions,omitempty"`
	Files           []*file     `json:"files,omitempty"`
	Upload          bool        `json:"upload,omitempty"`
	Hidden          bool        `json:"hidden,omitempty"`
	Attachments     []M         `json:"attachments,omitempty"`
	Blocks          []M         `json:"blocks,omitempty"`

	// Fields used by particular subtypes.
	DeletedTS       string   `json:"deleted_ts,omitempty"`
	ItemType        string   `json:"item_type,omitempty"`
	Message         *message `json:"message,omitempty"`
	PreviousMessage *message `json:"previous_message,omitempty"`
	Root            *message `json:"root,omitempty"`
	Room            M        `json:"room,omitempty"`
	Permalink       string   `json:"permalink,omitempty"`

	at stamp
}

// entry is one element of a day file: a message, or raw JSON that is
// deliberately not a usable message.
type entry struct {
	at  stamp
	msg *message
	raw json.RawMessage
}

type channel struct {
	id, name, kind string
	created        stamp
	creator        *user
	archived       bool
	general        bool
	members        []*user
	topic, purpose string

	entries  []entry
	usedMS   map[int64]bool
	reversed map[string]bool // day files written newest first
}

type builder struct {
	rng      *rand.Rand
	seq      int
	users    []*user
	byID     map[string]*user
	people   []Person
	channels []*channel
	canvases []M
	extra    map[string][]byte
}

func newBuilder(seed uint64) *builder {
	return &builder{
		rng:   rand.New(rand.NewPCG(seed, 0x7461757466697874)),
		byID:  map[string]*user{},
		extra: map[string][]byte{},
	}
}

// id returns the next Slack-style identifier with the given prefix letter.
func (b *builder) id(prefix string) string {
	b.seq++
	n := strings.ToUpper(strconv.FormatInt(int64(b.seq)*7919+46656, 36))
	return prefix + "0FX" + strings.Repeat("0", max(0, 6-len(n))) + n
}

type userSpec struct {
	first, last string
	display     string // overrides the first name
	deleted     bool
	bot         bool
	guest       bool
	ultra       bool
	stranger    bool
	external    bool // email at another organisation
	noEmail     bool
	unmapped    bool   // active, but with no matching directory account
	email       string // overrides the derived address
}

func (b *builder) addUser(s userSpec) *user {
	real := strings.TrimSpace(s.first + " " + s.last)
	handle := strings.ToLower(strings.ReplaceAll(real, " ", "."))
	display := s.first
	if s.display != "" {
		display = s.display
	}
	u := &user{
		ID:                b.id("U"),
		TeamID:            teamID,
		Name:              handle,
		Deleted:           s.deleted,
		RealName:          real,
		IsBot:             s.bot,
		IsAppUser:         s.bot,
		IsRestricted:      s.guest,
		IsUltraRestricted: s.ultra,
		IsStranger:        s.stranger,
		Updated:           baseTime.Unix() - 86400,
		Profile:           profile{RealName: real, DisplayName: display, Team: teamID},
	}
	switch {
	case s.bot:
		u.Profile.BotID = b.id("B")
	case s.noEmail || s.stranger:
	case s.email != "":
		u.Profile.Email = s.email
	case s.external || s.guest:
		u.Profile.Email = handle + "@" + partnerMail
	default:
		u.Profile.Email = handle + "@" + emailDomain
	}
	if s.stranger {
		u.TeamID = "T0PARTNER"
		u.Profile.Team = "T0PARTNER"
	}
	b.users = append(b.users, u)
	b.byID[u.ID] = u
	external := s.guest || s.stranger || s.external
	label := strings.TrimSpace(display)
	if label == "" {
		label = real
	}
	b.people = append(b.people, Person{
		SlackID:     u.ID,
		Email:       u.Profile.Email,
		DisplayName: label,
		Deleted:     s.deleted,
		Bot:         s.bot,
		External:    external,
		InDirectory: !(s.deleted || s.bot || external || s.noEmail || s.unmapped),
	})
	return u
}

func (b *builder) addChannel(kind, name string, creator *user, members ...*user) *channel {
	prefix := "C"
	if kind == "dm" {
		prefix = "D"
	}
	c := &channel{
		id:       b.id(prefix),
		name:     name,
		kind:     kind,
		created:  at(baseTime.AddDate(0, 0, -30)).add(time.Duration(len(b.channels)) * time.Minute),
		creator:  creator,
		members:  members,
		usedMS:   map[int64]bool{},
		reversed: map[string]bool{},
	}
	b.channels = append(b.channels, c)
	return c
}

func (c *channel) folder() string {
	if c.kind == "dm" {
		return c.id
	}
	return c.name
}

// post adds m at t, moving it forward a millisecond at a time until it does
// not share a millisecond with another message of the channel. Collisions
// therefore only exist where postExact creates them on purpose.
func (c *channel) post(t stamp, m *message) *message {
	for c.usedMS[t.ms()] {
		t += 1000
	}
	return c.postExact(t, m)
}

// postExact adds m at exactly t.
func (c *channel) postExact(t stamp, m *message) *message {
	c.usedMS[t.ms()] = true
	m.Type = "message"
	m.TS = t.String()
	m.at = t
	c.entries = append(c.entries, entry{at: t, msg: m})
	return m
}

// junk adds a day-file entry that is not a usable message, placed in the day
// of t.
func (c *channel) junk(t stamp, raw string) {
	c.entries = append(c.entries, entry{at: t, raw: json.RawMessage(raw)})
}

// by sets the author fields of m the way Slack does for a human user.
func by(u *user, m *message) *message {
	m.User = u.ID
	m.Team = u.TeamID
	m.UserTeam = u.TeamID
	m.SourceTeam = u.TeamID
	m.UserProfile = &msgProfile{
		DisplayName: u.Profile.DisplayName,
		RealName:    u.Profile.RealName,
		Name:        u.Name,
		Team:        u.TeamID,
	}
	return m
}

// reply posts r as a reply to parent at t, keeping the parent's thread
// summary fields in step.
func (c *channel) reply(parent *message, t stamp, r *message, exact bool) *message {
	parent.ThreadTS = parent.TS
	r.ThreadTS = parent.TS
	r.ParentUserID = parent.User
	if exact {
		c.postExact(t, r)
	} else {
		c.post(t, r)
	}
	parent.ReplyCount++
	parent.LatestReply = r.TS
	parent.Replies = append(parent.Replies, replyRef{User: r.User, TS: r.TS})
	if r.User != "" && !slices.Contains(parent.ReplyUsers, r.User) {
		parent.ReplyUsers = append(parent.ReplyUsers, r.User)
		parent.ReplyUsersCount = len(parent.ReplyUsers)
	}
	return r
}

func fakeToken(fileID string) string {
	return "xoxe-FAKE-fixture-not-a-secret-" + strings.ToLower(fileID)
}

// hostedFile returns a file uploaded to Slack, with realistic private URLs
// that embed a (fake) export token.
func (b *builder) hostedFile(u *user, t stamp, name, mimetype, filetype, pretty string, size int64) *file {
	id := b.id("F")
	slug := url.PathEscape(strings.ToLower(strings.ReplaceAll(name, " ", "_")))
	base := "https://files.slack.com/files-pri/" + teamID + "-" + id + "/"
	return &file{
		ID:                 id,
		Created:            t.sec(),
		Timestamp:          t.sec(),
		Name:               name,
		Title:              name,
		Mimetype:           mimetype,
		Filetype:           filetype,
		PrettyType:         pretty,
		User:               u.ID,
		UserTeam:           u.TeamID,
		Size:               size,
		Mode:               "hosted",
		URLPrivate:         base + slug + "?t=" + fakeToken(id),
		URLPrivateDownload: base + "download/" + slug + "?t=" + fakeToken(id),
		Permalink:          "https://" + workspace + ".slack.com/files/" + u.ID + "/" + id + "/" + slug,
	}
}

func (b *builder) pick(us []*user) *user { return us[b.rng.IntN(len(us))] }

// between returns a uniform integer in [lo, hi].
func (b *builder) between(lo, hi int) int { return lo + b.rng.IntN(hi-lo+1) }

func (b *builder) chance(percent int) bool { return b.rng.IntN(100) < percent }

func (b *builder) react(m *message, pool []*user, name string, n int) {
	n = min(n, len(pool))
	r := reaction{Name: name, Count: n}
	for _, i := range b.rng.Perm(len(pool))[:n] {
		r.Users = append(r.Users, pool[i].ID)
	}
	m.Reactions = append(m.Reactions, r)
}

func encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // Slack writes <@U123> literally
	enc.SetIndent("", "    ")
	if err := enc.Encode(v); err != nil {
		panic(fmt.Sprintf("fixtures: encoding generated data: %v", err))
	}
	return buf.Bytes()
}

func (c *channel) index() M {
	ids := make([]string, len(c.members))
	for i, u := range c.members {
		ids[i] = u.ID
	}
	if c.kind == "dm" {
		return M{"id": c.id, "created": c.created.sec(), "members": ids}
	}
	creator := ""
	if c.creator != nil {
		creator = c.creator.ID
	}
	note := func(v string) M {
		if v == "" {
			return M{"value": "", "creator": "", "last_set": 0}
		}
		return M{"value": v, "creator": creator, "last_set": c.created.sec()}
	}
	m := M{
		"id": c.id, "name": c.name, "created": c.created.sec(), "creator": creator,
		"is_archived": c.archived, "members": ids,
		"topic": note(c.topic), "purpose": note(c.purpose),
	}
	if c.kind == "public" {
		m["is_general"] = c.general
	}
	return m
}

// finish renders the export's files and counts the facts.
func (b *builder) finish() *built {
	out := &built{files: b.extra, people: b.people}
	f := &out.facts
	f.Subtypes = map[string]int{}
	f.PerChannel = map[string]ChannelFacts{}

	out.files["users.json"] = encode(b.users)
	f.Users = len(b.users)
	for i, u := range b.users {
		p := b.people[i]
		if u.Deleted {
			f.DeletedUsers++
		}
		if u.IsBot {
			f.Bots++
		}
		if u.IsRestricted || u.IsUltraRestricted {
			f.Guests++
		}
		if u.IsStranger {
			f.Strangers++
		}
		if !u.IsBot && u.Profile.Email == "" {
			f.UsersWithoutEmail++
		}
		if p.InDirectory {
			f.DirectoryUsers++
		}
	}

	index := map[string][]M{"public": {}} // channels.json is always present
	indexFile := map[string]string{"public": "channels.json", "private": "groups.json", "dm": "dms.json", "mpim": "mpims.json"}
	unknown := map[string]bool{}
	fileIDs := map[string]bool{}
	var first, last stamp

	for _, c := range b.channels {
		index[c.kind] = append(index[c.kind], c.index())
		f.Channels++
		switch c.kind {
		case "public":
			f.PublicChannels++
		case "private":
			f.PrivateChannels++
		case "dm":
			f.DMs++
		case "mpim":
			f.MPIMs++
		}
		if c.archived {
			f.ArchivedChannels++
		}
		if len(c.entries) == 0 {
			f.EmptyChannels++
		}
		cf := ChannelFacts{Name: c.name, Kind: c.kind, Folder: c.folder()}

		days := map[string][]entry{}
		perMS := map[int64]int{}
		threads := map[string]int{}
		for _, e := range c.entries {
			days[e.at.day()] = append(days[e.at.day()], e)
			if e.msg == nil {
				cf.Malformed++
				continue
			}
			m := e.msg
			cf.Messages++
			f.Subtypes[m.Subtype]++
			perMS[e.at.ms()]++
			if first == 0 || e.at < first {
				first = e.at
			}
			last = max(last, e.at)
			switch {
			case m.ThreadTS == "":
			case m.ThreadTS == m.TS:
				f.ThreadParents++
			default:
				f.Replies++
				threads[m.ThreadTS]++
			}
			if m.Edited != nil {
				f.EditedMessages++
			}
			if m.BotID != "" || m.Subtype == "bot_message" {
				f.BotMessages++
			}
			if len(m.Blocks) > 0 {
				f.MessagesWithBlocks++
			}
			if m.User != "" && b.byID[m.User] == nil {
				f.UnknownUserMessages++
				unknown[m.User] = true
			}
			f.LargestMessageBytes = max(f.LargestMessageBytes, len(m.Text))
			for _, r := range m.Reactions {
				f.Reactions++
				f.ReactionCount += r.Count
			}
			if len(m.Files) > 0 {
				f.MessagesWithFiles++
			}
			for _, fl := range m.Files {
				f.Files++
				f.FileBytes += fl.Size
				fileIDs[fl.ID] = true
				switch {
				case fl.HiddenByLimit || fl.Mode == "hidden_by_limit":
					f.HiddenFiles++
				case fl.Mode == "tombstone":
					f.TombstonedFiles++
				case fl.IsExternal:
					f.ExternalFiles++
				case fl.URLPrivateDownload != "":
					f.DownloadableFiles++
					f.DownloadableBytes += fl.Size
				}
				if fl.Mode == "space" {
					f.CanvasFiles++
				}
			}
		}
		for _, n := range perMS {
			f.SameMillisecondCollisions += n - 1
		}
		for _, n := range threads {
			f.LargestThread = max(f.LargestThread, n)
		}

		for day, entries := range days {
			slices.SortStableFunc(entries, func(a, b entry) int { return cmp.Compare(a.at, b.at) })
			if c.reversed[day] {
				slices.Reverse(entries)
			}
			list := make([]any, len(entries))
			for i, e := range entries {
				if e.msg != nil {
					list[i] = e.msg
				} else {
					list[i] = e.raw
				}
			}
			out.files[c.folder()+"/"+day+".json"] = encode(list)
		}
		cf.Days = len(days)
		f.DayFiles += cf.Days
		f.Messages += cf.Messages
		f.MalformedMessages += cf.Malformed
		f.PerChannel[c.id] = cf
	}

	for kind, list := range index {
		out.files[indexFile[kind]] = encode(list)
	}
	if len(b.canvases) > 0 {
		out.files["canvases.json"] = encode(b.canvases)
		f.Canvases = len(b.canvases)
	}
	f.UnknownAuthors = len(unknown)
	f.UniqueFiles = len(fileIDs)
	if f.Messages > 0 {
		f.FirstTS, f.LastTS = first.String(), last.String()
	}
	return out
}

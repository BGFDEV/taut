package fixtures

import (
	"archive/zip"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LoadPrefix starts the name of a load fixture: "load" for the defaults, or
// "load:" followed by a spec such as "channels=150,messages=200000".
const LoadPrefix = "load"

// LoadSpec describes a generated workspace for load tests. Unlike the three
// named fixtures it is written as a stream, one day file at a time, so its
// size is bounded by the disk and not by memory. It is as deterministic as
// they are: a spec always produces the same bytes.
//
// The shape is that of a real workspace rather than a uniform one: channel
// sizes fall off steeply (a few channels hold most of the messages and a
// long tail holds a handful each), days are busy on weekdays and nearly
// empty at weekends, a share of messages are thread replies and a share
// carry a file, some channels are private, and people are deactivated,
// unmapped or bots in the proportions that make an identity mapping work.
type LoadSpec struct {
	// Channels is the number of public and private channels together.
	Channels int
	// Messages is the exact number of messages in those channels.
	Messages int
	// Users is the number of entries in users.json.
	Users int
	// Days is the length of the history.
	Days int
	// Threads is the share of messages that are replies in a thread, and
	// Files the share that carry one file; both between 0 and 1.
	Threads float64
	Files   float64
	// Private is the share of channels that are private.
	Private float64
	// Skew is how steeply channel sizes fall off: a channel's share of the
	// messages is proportional to 1/rank^Skew. 0 makes every channel the
	// same size; the default, a little over 1, gives the largest channel
	// about a fifth of everything.
	Skew float64
	// DMs is the number of one-on-one conversations. One group conversation
	// is added for every five. DMMessages is the exact number of messages
	// in all of them together.
	DMs        int
	DMMessages int
	// Seed varies the content without changing the shape.
	Seed uint64
}

// DefaultLoadSpec is the load fixture when nothing is specified.
func DefaultLoadSpec() LoadSpec {
	return LoadSpec{Channels: 150, Messages: 200_000, Users: 400, Days: 365, Threads: 0.2, Files: 0.01, Private: 0.1, Skew: 1.05, Seed: 1}
}

// maxLoadDays keeps the history in the past: it starts when the other
// fixtures do, in March 2024.
const maxLoadDays = 800

// IsLoad reports whether name names a load fixture.
func IsLoad(name string) bool {
	return name == LoadPrefix || strings.HasPrefix(name, LoadPrefix+":")
}

// ParseLoadSpec reads a load fixture's name: "load", or "load:" and
// comma-separated key=value pairs over the defaults. The keys are channels,
// messages, users, days, threads, files, private, skew, dms, dmmessages and
// seed.
func ParseLoadSpec(name string) (LoadSpec, error) {
	s := DefaultLoadSpec()
	if !IsLoad(name) {
		return s, fmt.Errorf("fixtures: %q is not a load fixture", name)
	}
	_, rest, _ := strings.Cut(name, ":")
	dmMessages := -1
	for _, kv := range strings.Split(rest, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return s, fmt.Errorf("fixtures: load spec: %q is not key=value", kv)
		}
		whole := func(dst *int) error {
			n, err := strconv.Atoi(strings.ReplaceAll(v, "_", ""))
			if err != nil || n < 0 {
				return fmt.Errorf("fixtures: load spec: %s=%q is not a whole number", k, v)
			}
			*dst = n
			return nil
		}
		share := func(dst *float64) error {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || !(f >= 0 && f <= 1) {
				return fmt.Errorf("fixtures: load spec: %s=%q is not a share between 0 and 1", k, v)
			}
			*dst = f
			return nil
		}
		var err error
		switch strings.ToLower(k) {
		case "channels":
			err = whole(&s.Channels)
		case "messages":
			err = whole(&s.Messages)
		case "users":
			err = whole(&s.Users)
		case "days":
			err = whole(&s.Days)
		case "threads":
			err = share(&s.Threads)
		case "files":
			err = share(&s.Files)
		case "private":
			err = share(&s.Private)
		case "skew":
			f, perr := strconv.ParseFloat(v, 64)
			if perr != nil || !(f >= 0 && f <= 3) {
				err = fmt.Errorf("fixtures: load spec: skew=%q is not a number between 0 and 3", v)
			}
			s.Skew = f
		case "dms":
			err = whole(&s.DMs)
		case "dmmessages":
			err = whole(&dmMessages)
		case "seed":
			var n int
			err = whole(&n)
			s.Seed = uint64(n) //nolint:gosec // whole refuses negatives
		default:
			err = fmt.Errorf("fixtures: load spec: unknown key %q", k)
		}
		if err != nil {
			return s, err
		}
	}
	if dmMessages >= 0 {
		s.DMMessages = dmMessages
	} else {
		s.DMMessages = (s.DMs + s.DMs/5) * 150
	}
	return s, s.validate()
}

func (s LoadSpec) validate() error {
	groups := s.DMs / 5
	switch {
	case s.Channels < 1 || s.Channels > 20_000:
		return fmt.Errorf("fixtures: load spec: channels must be between 1 and 20000")
	case s.Messages < s.Channels:
		return fmt.Errorf("fixtures: load spec: messages must be at least the number of channels (%d)", s.Channels)
	case s.Users < 10 || s.Users > 200_000:
		return fmt.Errorf("fixtures: load spec: users must be between 10 and 200000")
	case s.Days < 1 || s.Days > maxLoadDays:
		return fmt.Errorf("fixtures: load spec: days must be between 1 and %d", maxLoadDays)
	case s.DMs > 0 && s.DMMessages < s.DMs+groups:
		return fmt.Errorf("fixtures: load spec: dmmessages must be at least the number of conversations (%d)", s.DMs+groups)
	case s.DMs == 0 && s.DMMessages > 0:
		return fmt.Errorf("fixtures: load spec: dmmessages needs dms")
	}
	if s.DMs > 0 {
		n := len(loadEligible(s))
		if n < 8 || s.DMs > n*((n-1)/2) || groups > n {
			return fmt.Errorf("fixtures: load spec: %d users cannot make %d distinct direct conversations", s.Users, s.DMs)
		}
	}
	return nil
}

// String is the spec as a fixture name ParseLoadSpec reads back.
func (s LoadSpec) String() string {
	return fmt.Sprintf("%s:channels=%d,messages=%d,users=%d,days=%d,threads=%g,files=%g,private=%g,skew=%g,dms=%d,dmmessages=%d,seed=%d",
		LoadPrefix, s.Channels, s.Messages, s.Users, s.Days, s.Threads, s.Files, s.Private, s.Skew, s.DMs, s.DMMessages, s.Seed)
}

// loadFileIDPrefix marks the files of a load fixture. Their sizes follow
// from their IDs (LoadFileSize), so a file host can serve them without
// reading the export.
const loadFileIDPrefix = "F0LD"

// LoadFileSize returns the size of a load fixture's file, and false for an
// ID that is not one.
func LoadFileSize(id string) (int64, bool) {
	if !strings.HasPrefix(id, loadFileIDPrefix) {
		return 0, false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return 2<<10 + int64(h.Sum32()%(62<<10)), true
}

var (
	loadFirst = []string{"Ada", "Ben", "Chloe", "Dev", "Elena", "Farid", "Grace", "Hugo", "Ines", "Jonas", "Keiko", "Liam", "Mira", "Noor", "Omar", "Priya",
		"Quinn", "Rosa", "Sven", "Tara", "Uma", "Viktor", "Wen", "Xavier", "Yara", "Zane", "Anya", "Bruno", "Carla", "Dmitri", "Esme", "Felix",
		"Gita", "Hana", "Ivan", "Jade", "Kofi", "Lena", "Marco", "Nadia"}
	loadLast = []string{"Abbott", "Banerjee", "Castro", "Dubois", "Eriksen", "Fischer", "Garcia", "Haddad", "Ito", "Jensen", "Kowalski", "Lindqvist", "Mbeki", "Novak", "Okafor", "Petrov",
		"Quispe", "Rossi", "Schmidt", "Tanaka", "Ueda", "Vasquez", "Weber", "Xu", "Yilmaz", "Zielinski", "Andersson", "Bianchi", "Chen", "Diaz", "Evans", "Fontaine",
		"Gupta", "Horvat", "Ivanova", "Jung", "Khan", "Lopez", "Moreau", "Nguyen"}
	loadAreas = []string{"eng", "design", "sales", "support", "ops", "data", "product", "marketing", "finance", "legal", "people", "infra", "mobile", "web", "platform", "security",
		"research", "partners", "billing", "growth"}
	loadTopics = []string{"general", "standup", "releases", "incidents", "reviews", "planning", "random", "help", "announce", "oncall", "roadmap", "feedback", "alerts", "onboarding", "retro", "ideas",
		"customers", "launch", "bugs", "docs"}
	loadWords = strings.Fields(`the a to of and in that is for it on with as was at by this have from or one had not but what all were we when your can
		said there use an each which she do how their if will up other about out many then them these so some her would make like him into time
		has look two more write go see number no way could people my than first water been call who oil its now find long down day did get come
		made may part release deploy review branch build test ticket customer meeting tomorrow today update status blocked done merged staging
		production rollback invoice quarter roadmap design feedback draft proposal budget hiring onboarding schedule estimate priority thanks
		please sorry agreed sounds good lunch coffee office remote calendar reminder deadline shipping pricing contract renewal dashboard metric
		latency error timeout retry cache database index query migration backup alert incident postmortem follow action owner`)
	loadEmoji = []string{"tada", "thumbsup", "eyes", "rocket", "pray", "white_check_mark", "joy", "fire"}
)

// loadUsers builds the users of a load workspace. It depends on the number
// of users only, so the people can be had without generating any message.
func loadUsers(s LoadSpec) ([]*user, []Person) {
	users := make([]*user, 0, s.Users)
	people := make([]Person, 0, s.Users)
	combos := len(loadFirst) * len(loadLast)
	for i := range s.Users {
		first, last := loadFirst[i%len(loadFirst)], loadLast[(i/len(loadFirst))%len(loadLast)]
		handle := strings.ToLower(first + "." + last)
		realName := first + " " + last
		if n := i / combos; n > 0 {
			handle += strconv.Itoa(n)
			realName += " " + strconv.Itoa(n)
		}
		// Fixed positions rather than draws, so the mix does not depend on
		// how many users there are. The first few are always ordinary.
		deleted := i%20 == 7
		bot := i%50 == 13
		unmapped := i%33 == 5 && !deleted && !bot
		u := &user{
			ID: fmt.Sprintf("U0LD%07X", i+1), TeamID: teamID, Name: handle, Deleted: deleted, RealName: realName,
			IsBot: bot, IsAppUser: bot, Updated: baseTime.Unix() - 86400,
			Profile: profile{RealName: realName, DisplayName: first, Team: teamID},
		}
		if bot {
			u.Profile.BotID = fmt.Sprintf("B0LD%07X", i+1)
		} else {
			u.Profile.Email = handle + "@" + emailDomain
		}
		users = append(users, u)
		people = append(people, Person{
			SlackID: u.ID, Email: u.Profile.Email, DisplayName: first, Deleted: deleted, Bot: bot,
			InDirectory: !deleted && !bot && !unmapped,
		})
	}
	return users, people
}

// loadEligible lists the indexes of the users a direct conversation can be
// between and still migrate: active people with a directory account.
func loadEligible(s LoadSpec) []int {
	var out []int
	for i := range s.Users {
		if i%20 != 7 && i%50 != 13 && i%33 != 5 {
			out = append(out, i)
		}
	}
	return out
}

// loadPeople returns the people of a load fixture.
func loadPeople(s LoadSpec) []Person {
	_, people := loadUsers(s)
	return people
}

// loadConversation is one channel or direct conversation to generate.
type loadConversation struct {
	*channel
	count int
	span  int // days of history
}

// share splits total over weights so that the parts add up to it exactly
// and each is at least one.
func share(total int, weights []float64) []int {
	sum := 0.0
	for _, w := range weights {
		sum += w
	}
	out := make([]int, len(weights))
	left := total - len(weights)
	given := 0
	for i, w := range weights {
		out[i] = int(float64(left) * w / sum)
		given += out[i]
	}
	for i := 0; given < left; i, given = (i+1)%len(out), given+1 {
		out[i]++
	}
	for i := range out {
		out[i]++
	}
	return out
}

func loadPlan(s LoadSpec) (users []*user, people []Person, convs []*loadConversation) {
	users, people = loadUsers(s)
	rng := rand.New(rand.NewPCG(s.Seed, 0x6c6f6164)) //nolint:gosec // determinism, not security
	created := at(baseTime.AddDate(0, 0, -30))

	// Channels: sizes fall off as a power of the rank, the first being the
	// largest.
	weights := make([]float64, s.Channels)
	for i := range weights {
		weights[i] = 1 / math.Pow(float64(i+1), s.Skew)
	}
	counts := share(s.Messages, weights)
	every := 0
	if s.Private > 0 {
		every = max(2, int(math.Round(1/s.Private)))
	}
	for i := range s.Channels {
		name := "general"
		if i > 0 {
			name = loadAreas[i%len(loadAreas)] + "-" + loadTopics[(i/len(loadAreas))%len(loadTopics)]
			if n := i / (len(loadAreas) * len(loadTopics)); n > 0 {
				name += "-" + strconv.Itoa(n)
			}
		}
		kind := "public"
		if every > 0 && i > 0 && i%every == every-1 {
			kind = "private"
		}
		// Bigger channels have more people in them; a window into the
		// user list that starts somewhere else for each channel.
		size := min(s.Users, max(3, 6+int(4*math.Sqrt(float64(counts[i])))))
		start := rng.IntN(s.Users)
		members := make([]*user, size)
		for j := range members {
			members[j] = users[(start+j)%s.Users]
		}
		span := s.Days
		if i >= 10 && s.Skew > 0 {
			span = max(1, int(float64(s.Days)*(0.25+0.75*rng.Float64())))
		}
		c := &channel{
			id: fmt.Sprintf("C0LD%07X", i+1), name: name, kind: kind, created: created.add(time.Duration(i) * time.Second),
			creator: members[0], members: members, general: i == 0, topic: "", purpose: "",
		}
		convs = append(convs, &loadConversation{channel: c, count: counts[i], span: span})
	}

	// Direct conversations, between people who can be migrated.
	if s.DMs > 0 {
		el := loadEligible(s)
		n := len(el)
		groups := s.DMs / 5
		weights := make([]float64, s.DMs+groups)
		for i := range weights {
			weights[i] = 1 / math.Pow(float64(i%97+1), 0.8)
		}
		counts := share(s.DMMessages, weights)
		for k := range s.DMs {
			a, off := k%n, 1+k/n
			pair := []*user{users[el[a]], users[el[(a+off)%n]]}
			c := &channel{id: fmt.Sprintf("D0LD%07X", k+1), kind: "dm", created: created.add(time.Duration(k) * time.Second), members: pair}
			convs = append(convs, &loadConversation{channel: c, count: counts[k], span: max(1, int(float64(s.Days)*(0.1+0.9*rng.Float64())))})
		}
		for j := range groups {
			// {a, a+2, a+5}: the gaps differ, so no two starts give the
			// same three people.
			members := []*user{users[el[j%n]], users[el[(j+2)%n]], users[el[(j+5)%n]]}
			names := make([]string, len(members))
			for i, u := range members {
				names[i] = u.Name
			}
			c := &channel{
				id: fmt.Sprintf("G0LD%07X", j+1), name: "mpdm-" + strings.Join(names, "--") + "-1", kind: "mpim",
				created: created.add(time.Duration(j) * time.Second), creator: members[0], members: members,
			}
			convs = append(convs, &loadConversation{channel: c, count: counts[s.DMs+j], span: max(1, int(float64(s.Days)*(0.1+0.9*rng.Float64())))})
		}
	}
	return users, people, convs
}

// loadText writes a message of a realistic length: mostly a line, now and
// then a paragraph.
func loadText(rng *rand.Rand, members []*user) string {
	n := 3 + rng.IntN(12)
	switch r := rng.IntN(100); {
	case r < 8:
		n = 40 + rng.IntN(80)
	case r < 30:
		n = 15 + rng.IntN(25)
	}
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(loadWords[rng.IntN(len(loadWords))])
		if i%13 == 12 && i < n-1 {
			b.WriteByte('.')
		}
	}
	switch r := rng.IntN(100); {
	case r < 4:
		b.WriteString(" <@" + members[rng.IntN(len(members))].ID + ">")
	case r < 7:
		b.WriteString(" <https://example.com/doc/" + strconv.Itoa(rng.IntN(100000)) + "|the doc>")
	case r < 12:
		b.WriteString(" :" + loadEmoji[rng.IntN(len(loadEmoji))] + ":")
	case r < 14:
		b.WriteString("\n```\nselect count(*) from orders where status = 'open';\n```")
	}
	return b.String()
}

// loadWriter streams a load fixture into a ZIP and counts what it writes.
type loadWriter struct {
	zw    *zip.Writer
	cfg   config
	facts FixtureFacts
	files int
	first stamp
	last  stamp
}

func (w *loadWriter) put(name string, data []byte) error {
	fw, err := w.zw.CreateHeader(&zip.FileHeader{Name: w.cfg.entry(name), Method: zip.Deflate, Modified: zipTime})
	if err != nil {
		return err
	}
	_, err = fw.Write(data)
	return err
}

// conversation generates one conversation's day files, in date order.
func (w *loadWriter) conversation(s LoadSpec, index int, c *loadConversation, byID map[string]*user) error {
	rng := rand.New(rand.NewPCG(s.Seed+uint64(index)+1, 0x636f6e76)) //nolint:gosec // determinism, not security
	cf := ChannelFacts{Name: c.name, Kind: c.kind, Folder: c.folder()}
	f := &w.facts

	// How many messages each day gets: weekdays busy, weekends nearly
	// empty, no two days alike. The conversation ends where the history
	// does and starts span days before.
	startDay := s.Days - c.span
	day := make([]float64, c.span)
	total := 0.0
	for d := range day {
		wd := baseTime.AddDate(0, 0, startDay+d).Weekday()
		day[d] = 0.4 + 1.2*rng.Float64()
		if wd == time.Saturday || wd == time.Sunday {
			day[d] *= 0.08
		}
		total += day[d]
	}
	var lastMS int64
	threads := map[string]int{}
	emitted, acc := 0, 0.0
	for d := range day {
		acc += day[d]
		n := int(math.Round(float64(c.count)*acc/total)) - emitted
		if d == len(day)-1 {
			n = c.count - emitted
		}
		if n <= 0 {
			continue
		}
		emitted += n
		// The day's messages, between 07:00 and 19:00, in order.
		open := at(baseTime.AddDate(0, 0, startDay+d).Add(-2 * time.Hour))
		offsets := make([]int64, n)
		for i := range offsets {
			offsets[i] = rng.Int64N(int64(12 * time.Hour / time.Microsecond))
		}
		slices.Sort(offsets)
		msgs := make([]*message, 0, n)
		var tops []*message
		for _, off := range offsets {
			t := open + stamp(off)
			if t.ms() <= lastMS {
				t = stamp((lastMS + 1) * 1000)
			}
			lastMS = t.ms()
			// A few people write most of what is said.
			r := rng.Float64()
			author := c.members[int(float64(len(c.members))*r*r)]
			m := &message{Type: "message", Text: loadText(rng, c.members), TS: t.String(), at: t}
			if author.IsBot {
				m.Subtype, m.BotID, m.Username = "bot_message", author.Profile.BotID, author.RealName
				f.BotMessages++
			} else {
				by(author, m)
			}
			if rng.IntN(100) < 15 {
				m.Blocks = []M{{"type": "rich_text", "block_id": "b" + strconv.Itoa(rng.IntN(1<<20)), "elements": []M{{
					"type": "rich_text_section", "elements": []M{{"type": "text", "text": m.Text}},
				}}}}
				f.MessagesWithBlocks++
			}
			if rng.IntN(100) < 8 {
				k := 1 + rng.IntN(min(4, len(c.members)))
				re := reaction{Name: loadEmoji[rng.IntN(len(loadEmoji))], Count: k}
				for j := range k {
					re.Users = append(re.Users, c.members[(j*7+rng.IntN(len(c.members)))%len(c.members)].ID)
				}
				m.Reactions = []reaction{re}
				f.Reactions++
				f.ReactionCount += k
			}
			if rng.IntN(100) < 3 && !author.IsBot {
				m.Edited = &edited{User: author.ID, TS: t.add(time.Minute).String()}
				f.EditedMessages++
			}
			if rng.Float64() < s.Files && !author.IsBot {
				w.files++
				id := fmt.Sprintf("%s%08X", loadFileIDPrefix, w.files)
				size, _ := LoadFileSize(id)
				kinds := [][4]string{{"notes-%d.txt", "text/plain", "text", "Plain Text"}, {"chart-%d.png", "image/png", "png", "PNG"}, {"report-%d.pdf", "application/pdf", "pdf", "PDF"}}
				k := kinds[rng.IntN(len(kinds))]
				name := fmt.Sprintf(k[0], w.files)
				base := "https://files.slack.com/files-pri/" + teamID + "-" + id + "/"
				m.Files = []*file{{
					ID: id, Created: t.sec(), Timestamp: t.sec(), Name: name, Title: name, Mimetype: k[1], Filetype: k[2], PrettyType: k[3],
					User: author.ID, UserTeam: author.TeamID, Size: size, Mode: "hosted",
					URLPrivate:         base + url.PathEscape(name) + "?t=" + fakeToken(id),
					URLPrivateDownload: base + "download/" + url.PathEscape(name) + "?t=" + fakeToken(id),
					Permalink:          "https://" + workspace + ".slack.com/files/" + author.ID + "/" + id + "/" + url.PathEscape(name),
				}}
				m.Upload = true
				f.MessagesWithFiles++
				f.Files++
				f.UniqueFiles++
				f.FileBytes += size
				f.DownloadableFiles++
				f.DownloadableBytes += size
			}
			// A reply answers one of the day's recent top-level messages.
			if len(tops) > 0 && rng.Float64() < s.Threads {
				parent := tops[len(tops)-1-rng.IntN(min(len(tops), 12))]
				if parent.ThreadTS == "" {
					parent.ThreadTS = parent.TS
					f.ThreadParents++
				}
				m.ThreadTS, m.ParentUserID = parent.TS, parent.User
				parent.ReplyCount++
				parent.LatestReply = m.TS
				parent.Replies = append(parent.Replies, replyRef{User: m.User, TS: m.TS})
				if m.User != "" && !slices.Contains(parent.ReplyUsers, m.User) {
					parent.ReplyUsers = append(parent.ReplyUsers, m.User)
					parent.ReplyUsersCount = len(parent.ReplyUsers)
				}
				f.Replies++
				threads[parent.TS]++
			} else {
				tops = append(tops, m)
			}
			if m.User != "" && byID[m.User] == nil {
				f.UnknownUserMessages++
			}
			f.Subtypes[m.Subtype]++
			f.LargestMessageBytes = max(f.LargestMessageBytes, len(m.Text))
			if w.first == 0 || t < w.first {
				w.first = t
			}
			w.last = max(w.last, t)
			msgs = append(msgs, m)
		}
		if err := w.put(c.folder()+"/"+open.add(2*time.Hour).day()+".json", encode(msgs)); err != nil {
			return err
		}
		cf.Days++
		cf.Messages += n
	}
	for _, n := range threads {
		f.LargestThread = max(f.LargestThread, n)
	}
	f.DayFiles += cf.Days
	f.Messages += cf.Messages
	f.PerChannel[c.id] = cf
	return nil
}

// writeLoad streams the load fixture to w and returns the truth about it.
func writeLoad(s LoadSpec, w io.Writer, cfg config) (FixtureFacts, []Person, error) {
	if err := s.validate(); err != nil {
		return FixtureFacts{}, nil, err
	}
	users, people, convs := loadPlan(s)
	lw := &loadWriter{zw: zip.NewWriter(w), cfg: cfg}
	f := &lw.facts
	f.Subtypes, f.PerChannel = map[string]int{}, map[string]ChannelFacts{}

	byID := make(map[string]*user, len(users))
	for i, u := range users {
		byID[u.ID] = u
		f.Users++
		if u.Deleted {
			f.DeletedUsers++
		}
		if u.IsBot {
			f.Bots++
		}
		if people[i].InDirectory {
			f.DirectoryUsers++
		}
	}
	if err := lw.put("users.json", encode(users)); err != nil {
		return *f, nil, err
	}
	index := map[string][]M{"public": {}}
	for _, c := range convs {
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
	}
	for _, kind := range slices.Sorted(maps.Keys(index)) {
		name := map[string]string{"public": "channels.json", "private": "groups.json", "dm": "dms.json", "mpim": "mpims.json"}[kind]
		if err := lw.put(name, encode(index[kind])); err != nil {
			return *f, nil, err
		}
	}
	for i, c := range convs {
		if err := lw.conversation(s, i, c, byID); err != nil {
			return *f, nil, err
		}
	}
	if f.Messages > 0 {
		f.FirstTS, f.LastTS = lw.first.String(), lw.last.String()
	}
	return *f, people, lw.zw.Close()
}

// WriteLoad streams the load fixture described by spec to w as a ZIP and
// returns the ground truth about what it wrote.
func WriteLoad(spec LoadSpec, w io.Writer, opts ...Option) (FixtureFacts, error) {
	f, _, err := writeLoad(spec, w, newConfig(opts))
	return f, err
}

// loadFacts caches the facts of load fixtures a process has asked about:
// counting them means generating the export once more, into nothing.
var loadFacts sync.Map // spec string → func() (FixtureFacts, error)

func factsOfLoad(name string) (FixtureFacts, error) {
	s, err := ParseLoadSpec(name)
	if err != nil {
		return FixtureFacts{}, err
	}
	once, _ := loadFacts.LoadOrStore(s.String(), sync.OnceValues(func() (FixtureFacts, error) {
		return WriteLoad(s, io.Discard)
	}))
	return once.(func() (FixtureFacts, error))()
}

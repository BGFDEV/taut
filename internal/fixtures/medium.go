package fixtures

import (
	"strings"
	"time"
)

type fileKind struct {
	ext, mimetype, filetype, pretty string
	minSize, maxSize                int
}

var fileKinds = []fileKind{
	{"png", "image/png", "png", "PNG", 20_000, 900_000},
	{"jpg", "image/jpeg", "jpg", "JPEG", 80_000, 4_000_000},
	{"pdf", "application/pdf", "pdf", "PDF", 30_000, 6_000_000},
	{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "docx", "Word Document", 12_000, 800_000},
	{"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "xlsx", "Excel Spreadsheet", 9_000, 2_000_000},
	{"mp4", "video/mp4", "mp4", "MPEG 4 Video", 2_000_000, 180_000_000},
	{"zip", "application/zip", "zip", "Zip", 50_000, 40_000_000},
	{"txt", "text/plain", "text", "Plain Text", 100, 20_000},
	{"csv", "text/csv", "csv", "CSV", 500, 3_000_000},
}

// buildMedium generates a realistic mid-size workspace: 40 users, public and
// private channels, DMs and group DMs, and about 5,000 messages over 90 days.
// The first 30 days are mrkdwn-only, as older clients wrote; later messages
// carry rich_text blocks alongside their text.
func buildMedium() *builder {
	b := newBuilder(2)

	var active, deleted, bots []*user
	for i := range 37 {
		spec := userSpec{first: firstNames[i], last: lastNames[(i*7+3)%len(lastNames)]}
		switch {
		case i >= 33:
			spec.deleted = true
		case i == 5:
			spec.guest = true
		case i == 6:
			spec.guest, spec.ultra = true, true
		case i == 11 || i == 19:
			spec.unmapped = true
		}
		u := b.addUser(spec)
		if spec.deleted {
			deleted = append(deleted, u)
		} else {
			active = append(active, u)
		}
	}
	for _, name := range []string{"deploybot", "pagerbot", "standup"} {
		bots = append(bots, b.addUser(userSpec{first: name, bot: true}))
	}

	type lane struct {
		c      *channel
		weight int
		bots   bool
	}
	var lanes []lane
	var public []*channel
	subset := func(n int) []*user {
		var out []*user
		for _, i := range b.rng.Perm(len(active))[:n] {
			out = append(out, active[i])
		}
		return out
	}
	publicNames := []string{"general", "random", "engineering", "design", "marketing", "sales",
		"support", "incidents", "announcements", "product", "data", "watercooler"}
	publicWeights := []int{20, 14, 10, 8, 8, 6, 6, 5, 4, 4, 3, 2}
	for i, name := range publicNames {
		members := subset(b.between(8, 20))
		if i < 2 {
			members = append(append([]*user{}, active...), deleted...)
		} else if i < 6 {
			members = append(members, deleted[i%len(deleted)])
		}
		c := b.addChannel("public", name, members[0], members...)
		c.general = i == 0
		c.purpose = "All things " + name
		if i%3 == 0 {
			c.topic = b.phrase(2, 5)
		}
		public = append(public, c)
		lanes = append(lanes, lane{c, publicWeights[i], name == "engineering" || name == "incidents" || name == "general"})
	}
	for i, name := range []string{"leadership", "hiring", "security-reviews"} {
		members := subset(b.between(4, 8))
		c := b.addChannel("private", name, members[0], members...)
		c.purpose = "Private: " + name
		lanes = append(lanes, lane{c, 3 - i/2, false})
	}
	for i := range 4 {
		pair := subset(2)
		if i == 3 {
			pair[1] = deleted[0] // a DM with someone who has since left
		}
		lanes = append(lanes, lane{b.addChannel("dm", "", nil, pair...), 1 + (3-i)/3, false})
	}
	for range 2 {
		members := subset(3)
		names := make([]string, len(members))
		for i, u := range members {
			names[i] = u.Name
		}
		c := b.addChannel("mpim", "mpdm-"+strings.Join(names, "--")+"-1", members[0], members...)
		c.purpose = "Group messaging"
		lanes = append(lanes, lane{c, 1, false})
	}
	total := 0
	for _, l := range lanes {
		total += l.weight
	}
	pickLane := func() lane {
		n := b.rng.IntN(total)
		for _, l := range lanes {
			if n -= l.weight; n < 0 {
				return l
			}
		}
		return lanes[0]
	}

	// Every channel opens with its creator joining.
	for _, l := range lanes {
		if c := l.c; c.creator != nil {
			c.post(at(baseTime).add(-time.Hour), &message{
				Subtype: "channel_join", User: c.creator.ID,
				Text: "<@" + c.creator.ID + "> has joined the channel",
			})
		}
	}

	midnight := baseTime.Truncate(24 * time.Hour)
	for d := range 90 {
		day := midnight.AddDate(0, 0, d)
		modern := d >= 30
		n := b.between(45, 55)
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			n = b.between(4, 10)
		}
		author := func(c *channel) *user {
			for {
				if u := b.pick(c.members); !u.Deleted || d < 30 {
					return u
				}
			}
		}
		for range n {
			l := pickLane()
			c := l.c
			t := at(day).add(time.Duration(8*3600+b.rng.IntN(11*3600))*time.Second +
				time.Duration(b.rng.IntN(1_000_000))*time.Microsecond)

			if l.bots && b.chance(8) {
				c.post(t, b.botMessage(b.pick(bots), modern))
				continue
			}

			u := author(c)
			m := by(u, &message{})
			if modern {
				b.richBody(m, c.members, public)
			} else {
				m.UserProfile = nil // old messages carry no profile snapshot
				b.mrkdwnBody(m, c.members)
			}
			c.post(t, m)

			if b.chance(4) {
				for range b.between(1, 3) {
					k := fileKinds[b.rng.IntN(len(fileKinds))]
					name := strings.ReplaceAll(b.phrase(1, 3), " ", "-") + "." + k.ext
					m.Files = append(m.Files, b.hostedFile(u, m.at, name, k.mimetype, k.filetype, k.pretty, int64(b.between(k.minSize, k.maxSize))))
				}
				m.Upload = true
			}
			if b.chance(10) {
				for _, i := range b.rng.Perm(len(emojiNames))[:b.between(1, 3)] {
					b.react(m, c.members, emojiNames[i], b.between(1, 5))
				}
			}
			if b.chance(4) {
				m.Edited = &edited{User: u.ID, TS: m.at.add(time.Duration(b.between(5, 900)) * time.Second).String()}
			}
			if b.chance(7) {
				rt := m.at
				for range b.between(1, 12) {
					rt = rt.add(time.Duration(b.between(30, 1500))*time.Second + time.Duration(b.rng.IntN(1_000_000))*time.Microsecond)
					r := by(author(c), &message{})
					if modern {
						b.richBody(r, c.members, public)
					} else {
						r.UserProfile = nil
						b.mrkdwnBody(r, c.members)
					}
					c.reply(m, rt, r, false)
				}
			}
		}
	}
	b.extra["integration_logs.json"] = encode([]M{{
		"app_id": "A0FX000APP", "app_type": "app", "change_type": "added",
		"date": "1709200000", "scope": "chat:write,channels:read",
		"user_id": active[0].ID, "user_name": active[0].Name,
	}})
	return b
}

// botMessage returns a message from an integration. Modern bots post as a
// bot user with blocks; legacy ones have only bot_id, username and
// attachments.
func (b *builder) botMessage(bot *user, modern bool) *message {
	if modern {
		m := &message{
			User: bot.ID, BotID: bot.Profile.BotID, Team: teamID,
			BotProfile: M{"id": bot.Profile.BotID, "name": bot.Name, "app_id": "A0FX000APP", "team_id": teamID},
		}
		return b.rich(m, section(styled(bot.Name, "bold"), tx(": "+b.sentence())))
	}
	return &message{
		Subtype: "bot_message", BotID: bot.Profile.BotID, Username: bot.Name,
		Text: b.sentence(),
		Attachments: []M{{
			"id": 1, "color": "36a64f", "fallback": b.sentence(),
			"title": b.phrase(2, 5), "title_link": b.url(), "text": b.sentence(),
			"fields": []M{{"title": "Status", "value": "ok", "short": true}},
		}},
	}
}

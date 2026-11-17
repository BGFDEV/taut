package fixtures

import (
	"fmt"
	"strings"
	"time"
)

// longChannelName is 80 characters, Slack's maximum and well past the 50
// that Teams allows.
var longChannelName = "proj-" + strings.Repeat("very-long-", 7) + "name0"

// buildNasty generates the workspace that breaks migrations. Each channel
// isolates one family of edge cases so a failing test points at its cause.
func buildNasty() *builder {
	b := newBuilder(3)
	t0 := at(baseTime)
	minute := func(n int) stamp { return t0.add(time.Duration(n) * time.Minute) }

	// --- People -----------------------------------------------------------
	ada := b.addUser(userSpec{first: "Ada", last: "Abbott"})
	ben := b.addUser(userSpec{first: "Ben", last: "Banerjee"})
	chloe := b.addUser(userSpec{first: "Chloe", last: "Castro", unmapped: true})
	// Slack kept the address in mixed case; directory matching must fold case.
	dev := b.addUser(userSpec{first: "Dev", last: "Dubois", email: "Dev.Dubois@Fixture.Example"})
	elena := b.addUser(userSpec{first: "Elena", last: "Eriksen", display: "Елена 🌻 (she/her)"})
	farid := b.addUser(userSpec{first: "Farid", last: "Fofana", guest: true})
	gone1 := b.addUser(userSpec{first: "Grace", last: "Garcia", deleted: true})
	gone2 := b.addUser(userSpec{first: "Hugo", last: "Hoffman", deleted: true})
	noMail := b.addUser(userSpec{first: "Ines", last: "Ito", display: " ", noEmail: true})
	stranger := b.addUser(userSpec{first: "Jonas", last: "Jensen", stranger: true})
	bot := b.addUser(userSpec{first: "buildbot", bot: true})
	active := []*user{ada, ben, chloe, dev, elena}
	everyone := b.users

	say := func(c *channel, u *user, t stamp, text string) *message {
		return c.post(t, b.rich(by(u, &message{}), section(tx(text))))
	}
	// plain posts mrkdwn with no blocks.
	plain := func(c *channel, u *user, t stamp, text string) *message {
		return c.post(t, by(u, &message{Text: text}))
	}

	// --- Channels whose names Teams rejects or reserves ---------------------
	general := b.addChannel("public", "general", ada, everyone...)
	general.general = true
	general.topic = "Company-wide <!here> & \"quotes\""
	for i, name := range []string{"_archive", "forms", "con", longChannelName} {
		c := b.addChannel("public", name, ada, active...)
		c.purpose = "Name that is legal in Slack and illegal or reserved in Teams"
		say(c, active[i], minute(10+i), "Posting in #"+name)
	}
	say(general, ada, minute(0), "Welcome to the nasty fixture.")
	general.post(minute(1), &message{Subtype: "channel_join", User: ben.ID, Text: "<@" + ben.ID + "> has joined the channel"})

	archived := b.addChannel("public", "old-project", gone1, ada, gone1, gone2)
	archived.archived = true
	say(archived, gone1, minute(-5000), "Kicking this off.")
	say(archived, gone2, minute(-4990), "On it.")
	archived.post(minute(-100), &message{Subtype: "channel_archive", User: ada.ID, Text: "<@" + ada.ID + "> archived the channel"})

	b.addChannel("public", "empty-room", ada, ada) // listed, but no folder

	// --- A thread with 2,500 replies ----------------------------------------
	mega := b.addChannel("public", "megathread", ada, active...)
	megaParent := say(mega, ada, minute(60), "Incident log: reply in thread only, please.")
	rt := megaParent.at
	for i := range 2500 {
		rt = rt.add(time.Duration(b.between(20, 100))*time.Second + time.Duration(b.rng.IntN(1_000_000))*time.Microsecond)
		r := b.rich(by(active[i%len(active)], &message{}), section(tx(fmt.Sprintf("Update %d: %s", i+1, b.sentence()))))
		mega.reply(megaParent, rt, r, false)
		if i%100 == 99 {
			r.Edited = &edited{User: r.User, TS: r.at.add(30 * time.Second).String()}
			b.react(r, active, "eyes", 1+i%5)
		}
	}

	// --- Messages colliding within one millisecond ---------------------------
	// Teams keeps millisecond precision, so these collide once migrated. The
	// day file is also written newest-first to exercise the reader's sort.
	coll := b.addChannel("public", "collisions", ada, active...)
	burst := minute(120) // on a whole second
	for i, micro := range []stamp{1, 2, 3, 4, 999} {
		coll.postExact(burst+micro, b.rich(by(active[i], &message{}), section(tx(fmt.Sprintf("burst %d", i+1)))))
	}
	collParent := coll.postExact(burst.add(time.Minute)+123_000, by(ada, &message{Text: "Parent posted at .123000"}))
	coll.reply(collParent, collParent.at+456, by(ben, &message{Text: "Reply within the same millisecond as its parent"}), true)
	coll.reply(collParent, collParent.at+999, by(chloe, &message{Text: "Second reply, still the same millisecond"}), true)
	coll.postExact(burst.add(2*time.Minute)+500_999, by(dev, &message{Text: "Last microsecond of .500"}))
	coll.postExact(burst.add(2*time.Minute)+501_000, by(elena, &message{Text: "First microsecond of .501 (no collision)"}))
	coll.reversed[burst.day()] = true
	// The same ts in another channel is not a collision.
	general.postExact(burst+1, by(ben, &message{Text: "Same ts as a message in #collisions"}))

	// --- Authors that are hard to map ------------------------------------------
	people := b.addChannel("public", "people", ada, everyone...)
	say(people, gone1, minute(200), "I have since been deactivated.")
	say(people, gone2, minute(201), "So have I.")
	say(people, noMail, minute(202), "My profile has no email and a blank display name.")
	say(people, stranger, minute(203), "I am a Slack Connect stranger listed in users.json.")
	say(people, farid, minute(204), "I am a guest.")
	say(people, elena, minute(205), "My display name is not ASCII.")
	// Absent from users.json: only the embedded profile names them.
	people.post(minute(206), &message{
		User: "U0ABSENT001", Team: "T0PARTNER", UserTeam: "T0PARTNER", SourceTeam: "T0PARTNER",
		Text:        "I am not in users.json at all; only user_profile knows my name.",
		UserProfile: &msgProfile{DisplayName: "Kai (Partner Co)", RealName: "Kai Kowalski", Name: "kai", Team: "T0PARTNER"},
	})
	people.post(minute(207), &message{
		User: "U0ABSENT002", UserTeam: "T0PARTNER",
		Text:        "Absent too, and my display name is empty.",
		UserProfile: &msgProfile{DisplayName: "", RealName: "Lena Larsen", Team: "T0PARTNER"},
	})
	people.post(minute(208), &message{User: "U0ABSENT003", Text: "Absent, with no profile snapshot either."})
	// Bots.
	people.post(minute(209), &message{Subtype: "bot_message", BotID: "B0LEGACY01", Username: "jenkins", Text: "Build #512 passed"})
	people.post(minute(210), &message{Subtype: "bot_message", BotID: "B0LEGACY02", Username: "", Text: "",
		Attachments: []M{{"id": 1, "fallback": "Deploy finished", "text": "Deploy *finished* in <https://ci.fixture.example/run/9|run 9>", "color": "danger", "mrkdwn_in": []string{"text"}}}})
	people.post(minute(211), b.rich(&message{User: bot.ID, BotID: bot.Profile.BotID, Team: teamID}, section(tx("Posted by a bot user that is in users.json"))))
	people.post(minute(212), &message{Subtype: "slackbot_response", User: slackbotID, Text: "I can help with that!"})

	// --- Files ---------------------------------------------------------------
	files := b.addChannel("public", "files", ada, active...)
	attach := func(m *message, f ...*file) {
		m.Files = append(m.Files, f...)
		m.Upload = true
	}
	big := say(files, ada, minute(300), "Recording of the all-hands.")
	attach(big, b.hostedFile(ada, big.at, "all-hands-recording.mp4", "video/mp4", "mp4", "MPEG 4 Video", 1<<30))
	huge := say(files, ben, minute(301), "Database dump, do not lose.")
	attach(huge, b.hostedFile(ben, huge.at, "db-dump.tar.gz", "application/gzip", "gzip", "Gzip", 5<<30))

	gdrive := say(files, chloe, minute(302), "Budget sheet lives in Drive.")
	gid := b.id("F")
	gdrive.Files = []*file{{
		ID: gid, Created: gdrive.at.sec(), Timestamp: gdrive.at.sec(),
		Name: "FY24 Budget", Title: "FY24 Budget",
		Mimetype: "application/vnd.google-apps.spreadsheet", Filetype: "gsheet", PrettyType: "Google Sheets",
		User: chloe.ID, Mode: "external", IsExternal: true, ExternalType: "gdrive",
		ExternalURL: "https://docs.google.com/spreadsheets/d/FAKE-FIXTURE-SHEET/edit",
		URLPrivate:  "https://docs.google.com/spreadsheets/d/FAKE-FIXTURE-SHEET/edit",
		Permalink:   "https://" + workspace + ".slack.com/files/" + chloe.ID + "/" + gid + "/fy24_budget",
	}}

	hidden := say(files, dev, minute(303), "Two files beyond the free plan's history limit.")
	hidden.Files = []*file{
		{ID: b.id("F"), Mode: "hidden_by_limit"},
		{ID: b.id("F"), HiddenByLimit: true},
	}
	hidden.Upload = true

	tomb := say(files, elena, minute(304), "This file was deleted later.")
	tomb.Files = []*file{{ID: b.id("F"), Mode: "tombstone"}}

	canvasMsg := say(files, ada, minute(305), "Launch plan canvas.")
	cid := b.id("F")
	canvasFile := &file{
		ID: cid, Created: canvasMsg.at.sec(), Timestamp: canvasMsg.at.sec(),
		Name: "Launch plan", Title: "Launch plan",
		Mimetype: "application/vnd.slack-docs", Filetype: "quip", PrettyType: "Canvas",
		User: ada.ID, UserTeam: teamID, Size: 18_442, Mode: "space",
		URLPrivate: "https://files.slack.com/files-pri/" + teamID + "-" + cid + "/launch_plan?t=" + fakeToken(cid),
		Permalink:  "https://" + workspace + ".slack.com/docs/" + teamID + "/" + cid,
	}
	canvasMsg.Files = []*file{canvasFile}
	b.canvases = append(b.canvases, M{
		"id": cid, "title": "Launch plan", "owner": ada.ID,
		"created": canvasMsg.at.sec(), "date_updated": canvasMsg.at.sec() + 3600,
		"channels": []string{files.id},
	})

	odd := say(files, ben, minute(306), "Awkward file names, an empty file and several attachments at once.")
	shared := b.hostedFile(ben, odd.at, "résumé (final) #2 100%.pdf", "application/pdf", "pdf", "PDF", 73_519)
	attach(odd,
		shared,
		b.hostedFile(ben, odd.at, "../../escape.txt", "text/plain", "text", "Plain Text", 12),
		b.hostedFile(ben, odd.at, strings.Repeat("long-name-", 25)+".csv", "text/csv", "csv", "CSV", 2_048),
		b.hostedFile(ben, odd.at, "empty.bin", "application/octet-stream", "binary", "Binary", 0),
		b.hostedFile(ben, odd.at, "日本語のファイル名 📎.png", "image/png", "png", "PNG", 204_800),
	)
	// The same file shared again: one file ID under two messages.
	again := plain(files, ada, minute(307), "")
	attach(again, shared)

	// --- Unicode ---------------------------------------------------------------
	uni := b.addChannel("public", "unicode", ada, active...)
	uni.post(minute(400), b.rich(by(ada, &message{}), section(
		inline{M{"type": "emoji", "name": "wave", "skin_tone": 3, "unicode": "1f44b-1f3fc"}, ":wave::skin-tone-3:"},
		tx(" hello "),
		inline{M{"type": "emoji", "name": "+1", "skin_tone": 6, "unicode": "1f44d-1f3ff"}, ":+1::skin-tone-6:"},
		tx(" "),
		inline{M{"type": "emoji", "name": "partyparrot"}, ":partyparrot:"}, // custom emoji: no unicode
	)))
	for i, text := range []string{
		"ZWJ sequences: 👩‍👩‍👧‍👦 👨🏽‍💻 🏳️‍🌈 🧑‍🚀 and a keycap 1️⃣",
		"RTL: مرحبا بالعالم mixed with LTR abc and שלום עולם, plus an override \u202Edesrever\u202C",
		"Combining marks: e\u0301 vs é, a\u0308\u0304, Z\u0336\u0322a\u0334l\u0335g\u0336o\u0337, and ก็ไม่ได้",
		"CJK: 日本語のテキスト、中文文本，한국어 텍스트、𠮷野家 (astral)",
		"Invisible: zero\u200Bwidth, word\u2060joiner, BOM\uFEFFinside, line\u2028separator, nbsp\u00A0here",
		"Math and fancy: 𝔘𝔫𝔦𝔠𝔬𝔡𝔢 𝓯𝓪𝓷𝓬𝔂 ∑∫√ ½ ﬁ ǅ İi ß",
		"Tabs\tand\r\nCRLF and a \\ backslash and \"quotes\" and a lone ampersand &",
	} {
		say(uni, active[i%len(active)], minute(401+i), text)
	}
	// Multi-byte filler so that cutting at a byte limit lands inside a rune.
	filler := func(size int) string {
		var sb strings.Builder
		for sb.Len() < size {
			sb.WriteString(b.sentence())
			sb.WriteString(" — ünïcödé 日本 🚀 ")
		}
		return sb.String()
	}
	say(uni, ada, minute(420), filler(60<<10))
	plain(uni, ben, minute(421), filler(150<<10))

	// --- Subtypes and edits ------------------------------------------------------
	sub := b.addChannel("public", "subtypes", ada, active...)
	sub.post(minute(500), &message{Subtype: "channel_join", User: chloe.ID, Text: "<@" + chloe.ID + "> has joined the channel"})
	editedMsg := say(sub, ada, minute(501), "This message was edited once.")
	editedMsg.Edited = &edited{User: ada.ID, TS: minute(503).String()}
	// An edit long after the fact, by someone who has since left.
	oldEdit := say(sub, gone1, minute(502), "Edited weeks later.")
	oldEdit.Edited = &edited{User: gone1.ID, TS: minute(40_000).String()}
	sub.post(minute(503), &message{
		Subtype: "message_changed", Hidden: true,
		Message:         &message{Type: "message", User: ada.ID, Text: "This message was edited once.", TS: editedMsg.TS, Edited: editedMsg.Edited},
		PreviousMessage: &message{Type: "message", User: ada.ID, Text: "This mesage was edited once.", TS: editedMsg.TS},
	})
	sub.post(minute(504), &message{Subtype: "message_deleted", Hidden: true, DeletedTS: minute(499).String()})
	pinned := sub.post(minute(505), &message{Subtype: "pinned_item", User: ben.ID, ItemType: "C", Text: "<@" + ben.ID + "> pinned a message to this channel."})
	b.react(pinned, active, "pushpin", 2)

	threadRoot := say(sub, ada, minute(510), "Thread with a broadcast reply.")
	sub.reply(threadRoot, minute(511), b.rich(by(ben, &message{}), section(tx("Quiet reply."))), false)
	sub.reply(threadRoot, minute(512), b.rich(by(chloe, &message{
		Subtype: "thread_broadcast",
		Root:    &message{Type: "message", User: ada.ID, Text: threadRoot.Text, TS: threadRoot.TS, ThreadTS: threadRoot.TS},
	}), section(tx("Also sent to the channel."))), false)

	// The parent was deleted but its replies survive: Slack leaves a tombstone.
	tombRoot := sub.post(minute(520), &message{Subtype: "tombstone", User: slackbotID, Text: "This message was deleted.", Hidden: true})
	sub.reply(tombRoot, minute(521), b.rich(by(dev, &message{}), section(tx("Replying to something that is now gone."))), false)
	sub.reply(tombRoot, minute(522), b.rich(by(elena, &message{}), section(tx("Same."))), false)

	sub.post(minute(530), &message{
		Subtype: "huddle_thread", User: slackbotID, Text: "A huddle started",
		Permalink: "https://" + workspace + ".slack.com/call/R0FX0HUDDLE",
		Room: M{
			"id": "R0FX0HUDDLE", "name": "", "channels": []string{sub.id}, "created_by": ada.ID,
			"date_start": minute(530).sec(), "date_end": minute(545).sec(),
			"participant_history": []string{ada.ID, ben.ID, "U0ABSENT001"},
			"has_ended":           true, "is_dm_call": false,
		},
	})
	sub.post(minute(531), &message{Subtype: "taut_fixture_unknown_subtype", User: ada.ID, Text: "A subtype nobody has seen before."})
	sub.post(minute(532), &message{Subtype: "channel_topic", User: ada.ID, Text: "<@" + ada.ID + "> set the channel topic: ship it"})
	multi := say(sub, ben, minute(533), "Lots of reactions, some from people who are gone.")
	b.react(multi, everyone, "+1", 7)
	b.react(multi, []*user{gone1, gone2}, "+1::skin-tone-4", 2)
	b.react(multi, active, "partyparrot", 1)
	multi.Reactions = append(multi.Reactions, reaction{Name: "eyes", Users: []string{"U0ABSENT001", ada.ID}, Count: 3}) // count disagrees with users

	// --- mrkdwn edge cases (text only, no blocks) --------------------------------
	md := b.addChannel("public", "mrkdwn", ada, active...)
	for i, text := range []string{
		"*bold _italic ~strike `code`~_* and *bold*_italic_~strike~ with no spaces",
		"Unbalanced *bold, snake_case_name, 2*3*4, a~b, `unterminated and ~~~",
		"<!date^1709546400^{date_short} at {time}|Mar 4, 2024 at 10:00 AM> and <!date^1709546400^{date_num}^https://wiki.fixture.example/cal|fallback>",
		"<!subteam^S0FXENG001|@eng> <!subteam^S0FXOPS002> please look",
		"<!here> <!channel> <!everyone> <!here|here> <!channel|@channel>",
		"Entities: 1 &lt; 2 &amp;&amp; 3 &gt; 2, literal &amp;lt; and a bare & too",
		"<https://wiki.fixture.example/a|label | with | pipes> <https://wiki.fixture.example/?q=a%7Cb&x=1|query> <https://wiki.fixture.example>",
		"<mailto:ada.abbott@fixture.example|ada.abbott@fixture.example> <tel:+15555550100|call me> <slack://channel?team=T0FIXTURE&id=C0FX|deep link>",
		"Mentions: <@" + ben.ID + "> <@" + ben.ID + "|ben> <@U0ABSENT001> <@" + gone1.ID + "> <#" + general.id + "|general> <#" + general.id + "> <#C0GONE0000|deleted-channel>",
		"```*not bold* _not italic_ <@" + ada.ID + "> :smile: &lt;tag&gt;\n  indented\n```",
		"Inline `*not bold*` and ``double`` and `<https://ci.fixture.example|x>`",
		"&gt; quoted line\n&gt; second quoted line\nnot quoted\n&gt;&gt;&gt; everything after\nis quoted",
		"• bullet by hand\n• second\n    ◦ nested\n1. ordered by hand\n2. second",
		":+1::skin-tone-6: :not-an-emoji: :: :a:b: :100: time 10:30:45",
		"Line one\n\n\n\nLine five after blank lines   and   runs   of   spaces ",
		"*<https://wiki.fixture.example/x|bold link>* _<@" + chloe.ID + ">_ ~<!here>~",
		"<script>alert(1)</script> &lt;script&gt;alert(2)&lt;/script&gt; <img src=x onerror=alert(3)>",
		"",
	} {
		plain(md, active[i%len(active)], minute(600+i), text)
	}

	// --- Malformed day-file entries ----------------------------------------------
	bad := b.addChannel("public", "malformed", ada, active...)
	plain(bad, ada, minute(700), "Valid message before the damage.")
	bad.junk(minute(701), fmt.Sprintf(`{"type":"message","user":%q,"text":"ts is a number, not a string","ts":%d.000200}`, ada.ID, minute(701).sec()))
	bad.junk(minute(702), fmt.Sprintf(`{"type":"message","user":%q,"text":"no ts at all"}`, ben.ID))
	plain(bad, ben, minute(703), "Valid message between damaged entries.")
	bad.junk(minute(704), `"just a string, not an object"`)
	bad.junk(minute(705), `null`)
	bad.junk(minute(706), fmt.Sprintf(`{"type":"message","user":%q,"text":"ts is not a timestamp","ts":"yesterday-ish"}`, chloe.ID))
	bad.junk(minute(707), fmt.Sprintf(`{"type":"message","ts":%q,"text":{"unexpected":"object"},"files":"nope"}`, minute(707).String()))
	plain(bad, chloe, minute(708), "Valid message after the damage.")
	plain(bad, dev, minute(708+24*60), "Valid message on the following day.")

	// --- Private channel, DMs and a group DM -----------------------------------
	secret := b.addChannel("private", "secret-plans", ada, ada, ben)
	say(secret, ada, minute(800), "Private channel message.")
	dm := b.addChannel("dm", "", nil, ada, ben)
	say(dm, ada, minute(810), "DM message.")
	say(dm, ben, minute(811), "DM reply (not threaded).")
	goneDM := b.addChannel("dm", "", nil, ada, gone1)
	say(goneDM, gone1, minute(-3000), "DM from someone who has since left.")
	selfDM := b.addChannel("dm", "", nil, ada)
	plain(selfDM, ada, minute(812), "Note to self.")
	mpim := b.addChannel("mpim", "mpdm-ada.abbott--ben.banerjee--jonas.jensen-1", ada, ada, ben, stranger)
	say(mpim, stranger, minute(820), "Group DM that includes a Slack Connect stranger.")

	// --- Direct messages on their way into Teams chats ---------------------------
	// Above: a plain DM between two people who both have an account (ada and
	// ben), one with someone who has left, one with oneself and a group DM
	// with a stranger. Below, the rest of what decides whether, and how, a
	// direct message can be imported into a chat.
	//
	// Conversations that cannot become a chat, each for its own reason: a
	// bot, a guest, and an active person with no account in the directory.
	botDM := b.addChannel("dm", "", nil, ada, bot)
	say(botDM, ada, minute(830), "DM to a bot.")
	guestDM := b.addChannel("dm", "", nil, ada, farid)
	say(guestDM, farid, minute(831), "DM with a guest.")
	unmappedDM := b.addChannel("dm", "", nil, ada, chloe)
	say(unmappedDM, chloe, minute(832), "DM with someone who has no Microsoft 365 account.")

	// A DM whose thread replies interleave with other messages, on two
	// days. A chat has no threads, so the order in time is all that is
	// left of the thread. It also carries a file and an inline image, and
	// two messages within one millisecond.
	threadDM := b.addChannel("dm", "", nil, dev, elena)
	root := say(threadDM, dev, minute(840), "Thread root in a DM.")
	say(threadDM, elena, minute(841), "Unthreaded message between the replies.")
	threadDM.reply(root, minute(842), b.rich(by(elena, &message{}), section(tx("First thread reply."))), false)
	say(threadDM, dev, minute(843), "Another unthreaded message.")
	threadDM.reply(root, minute(844), by(dev, &message{Text: "Second thread reply."}), false)
	withFiles := say(threadDM, elena, minute(845), "Here is the report and a screenshot.")
	withFiles.Files = []*file{
		b.hostedFile(elena, withFiles.at, "dm report.pdf", "application/pdf", "pdf", "PDF", 20_480),
		b.hostedFile(elena, withFiles.at, "dm screenshot.png", "image/png", "png", "PNG", 9_216),
	}
	withFiles.Upload = true
	sameMS := minute(846)
	threadDM.postExact(sameMS+100, by(dev, &message{Text: "First of two messages in one millisecond."}))
	threadDM.postExact(sameMS+900, by(elena, &message{Text: "Second of two messages in one millisecond."}))
	// A bot speaks in the conversation without being a participant of it.
	say(threadDM, bot, minute(847), "Reminder posted by a bot inside a DM.")
	// And again, six times in a row: more refusals than a lane tolerates in
	// a channel, if Microsoft will not take a message from a non-member.
	for i := range 6 {
		say(threadDM, bot, minute(848).add(time.Duration(i)*time.Second), fmt.Sprintf("Bot reminder %d inside a DM.", i+2))
	}
	// A mention of someone who is not in the conversation.
	plain(threadDM, dev, minute(850), "Ask <@"+ada.ID+"> about the report.")
	// A message long enough to be split into two posts, a millisecond
	// apart: its parts must stay together whatever happens to the first.
	plain(threadDM, dev, minute(849), filler(100<<10))
	threadDM.reply(root, minute(840+24*60), by(elena, &message{Text: "Thread reply on the following day."}), false)
	say(threadDM, dev, minute(841+24*60), "Unthreaded message on the following day.")

	// A group DM in which everybody has an account. Its name and purpose
	// are the ones Slack generates, so the chat gets no name.
	group := b.addChannel("mpim", "mpdm-ada.abbott--ben.banerjee--dev.dubois-1", ada, ada, ben, dev)
	group.purpose = "Group messaging with: @ada.abbott @ben.banerjee @dev.dubois"
	say(group, ada, minute(850), "Group DM message.")
	say(group, dev, minute(851), "Group DM answer.")
	groupReply := say(group, ben, minute(852), "Group DM thread root.")
	group.reply(groupReply, minute(853), by(ada, &message{Text: "Group DM thread reply."}), false)

	return b
}

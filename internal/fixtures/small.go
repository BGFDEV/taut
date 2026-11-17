package fixtures

import (
	"fmt"
	"time"
)

var firstNames = []string{
	"Ada", "Ben", "Chloe", "Dev", "Elena", "Farid", "Grace", "Hugo", "Ines", "Jonas",
	"Keiko", "Liam", "Maya", "Nico", "Olga", "Pablo", "Quinn", "Rosa", "Sam", "Tara",
	"Umar", "Vera", "Wes", "Xena", "Yusuf", "Zoe", "Arjun", "Bea", "Cyrus", "Dana",
	"Emil", "Freya", "Gus", "Hana", "Ivan", "Jade", "Kofi", "Lena", "Milo", "Nora",
}

var lastNames = []string{
	"Abbott", "Banerjee", "Castro", "Dubois", "Eriksen", "Fofana", "Garcia", "Hoffman", "Ito", "Jensen",
	"Kowalski", "Lopez", "Mbeki", "Nguyen", "Okafor", "Petrov", "Quigley", "Rossi", "Silva", "Tanaka",
	"Ueda", "Vargas", "Weber", "Xu", "Yilmaz", "Zimmer", "Ahmed", "Brooks", "Chen", "Diaz",
	"Engel", "Fischer", "Gupta", "Haddad", "Ivanov", "Jung", "Kaya", "Larsen", "Moreau", "Novak",
}

// buildSmall generates a tiny, clean workspace: five people, three public
// channels, four days of chat, one thread, one file and one reaction.
func buildSmall() *builder {
	b := newBuilder(1)
	var people []*user
	for i := range 5 {
		people = append(people, b.addUser(userSpec{first: firstNames[i], last: lastNames[i]}))
	}

	general := b.addChannel("public", "general", people[0], people...)
	general.general = true
	general.purpose = "Company-wide announcements and work-based matters"
	random := b.addChannel("public", "random", people[0], people...)
	random.purpose = "Non-work banter and water cooler conversation"
	atlas := b.addChannel("public", "project-atlas", people[1], people[:4]...)
	atlas.topic = "Atlas launch"
	chans := []*channel{general, random, atlas}

	var all []*message
	for d := range 4 {
		day := at(baseTime.AddDate(0, 0, d))
		for ci, c := range chans {
			for i := range 5 {
				t := day.add(time.Duration(i*47+ci*7)*time.Minute + time.Duration(b.rng.IntN(60_000_000))*time.Microsecond)
				m := by(c.members[(d+i+ci)%len(c.members)], &message{})
				all = append(all, c.post(t, b.richBody(m, c.members, chans)))
			}
		}
	}

	// One thread in #project-atlas, started on the first day.
	parent := atlas.entries[1].msg
	for i := range 3 {
		r := by(atlas.members[(i+2)%4], &message{})
		b.rich(r, section(tx(b.sentence())))
		atlas.reply(parent, parent.at.add(time.Duration(i+1)*4*time.Minute), r, false)
	}

	// One file and one reaction.
	withFile := general.entries[7].msg
	withFile.Files = []*file{b.hostedFile(b.byID[withFile.User], withFile.at, "Q2 plan.pdf", "application/pdf", "pdf", "PDF", 48_213)}
	withFile.Upload = true
	b.react(all[20], people, "tada", 3)

	// A direct message and a group DM, on two of the days. They are added
	// last, so everything above is byte for byte what it was without them.
	dm := b.addChannel("dm", "", nil, people[0], people[1])
	group := b.addChannel("mpim", "mpdm-ada.abbott--ben.banerjee--chloe.castro-1", people[0], people[:3]...)
	group.purpose = "Group messaging with: @ada.abbott @ben.banerjee @chloe.castro"
	for d := range 2 {
		day := at(baseTime.AddDate(0, 0, d))
		for i := range 3 {
			t := day.add(time.Duration(200+i*13) * time.Minute)
			dm.post(t, by(people[(d+i)%2], &message{Text: fmt.Sprintf("Direct message %d of day %d.", i+1, d+1)}))
			group.post(t.add(time.Minute), by(people[(d+i)%3], &message{Text: fmt.Sprintf("Group message %d of day %d.", i+1, d+1)}))
		}
	}
	return b
}

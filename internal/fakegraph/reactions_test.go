package fakegraph

import (
	"fmt"
	"strings"
	"testing"
)

const testUser2 = "a0000000-0000-4000-8000-000000000002"

func reaction(kind, user string) string {
	return fmt.Sprintf(`{"reactionType":%q,"createdDateTime":"2019-02-04T19:58:15.511Z","user":{"user":{"id":%q,"userIdentityType":"aadUser"}}}`, kind, user)
}

func withReactions(when string, reactions ...string) string {
	return importBody(when, `,"reactions":[`+strings.Join(reactions, ",")+`]`)
}

// Reactions on an import follow the chatMessageReaction reference: a
// reactionType that is a Unicode emoji, "custom" or a legacy name, and a
// user in an identity set of which only user is set. The import guide puts
// up to 250 of them in scope.
func TestImportReactionsWire(t *testing.T) {
	h := newHarness(t, DefaultSeed())
	token := h.token()
	msgs := chanPath + "/messages"
	if resp, _ := h.post(chanPath+"/startMigration", token, `{"conversationCreationDateTime":"2015-01-01T00:00:00Z"}`); resp.StatusCode != 204 {
		t.Fatalf("startMigration: %d", resp.StatusCode)
	}

	many := func(n int) []string {
		// One reaction per user and emoji is not something the reference
		// asks for, so the fake does not check it either.
		out := make([]string, n)
		for i := range out {
			out[i] = reaction("👍", testUser)
		}
		return out
	}
	const when = "2019-02-04T19:58:15.511Z"
	for name, body := range map[string]string{
		"over the limit":      withReactions(when, many(MaxImportReactions+1)...),
		"no type":             withReactions(when, `{"user":{"user":{"id":"`+testUser+`"}}}`),
		"empty type":          withReactions(when, reaction("", testUser)),
		"a shortcode":         withReactions(when, reaction("thumbsup", testUser)),
		"a shortcode, colons": withReactions(when, reaction(":+1:", testUser)),
		"emoji and text":      withReactions(when, reaction("👍 yes", testUser)),
		"plain punctuation":   withReactions(when, reaction("+1", testUser)),
		"too long":            withReactions(when, reaction(strings.Repeat("👍", 20), testUser)),
		"custom without url":  withReactions(when, reaction("custom", testUser)),
		"no user":             withReactions(when, `{"reactionType":"👍"}`),
		"null user":           withReactions(when, `{"reactionType":"👍","user":{"user":null}}`),
		"user without id":     withReactions(when, `{"reactionType":"👍","user":{"user":{"displayName":"Ada"}}}`),
		"unknown user":        withReactions(when, reaction("👍", "not-a-user")),
		"an application":      withReactions(when, `{"reactionType":"👍","user":{"application":{"id":"app"},"user":{"id":"`+testUser+`"}}}`),
		"a device":            withReactions(when, `{"reactionType":"👍","user":{"device":{"id":"d"},"user":{"id":"`+testUser+`"}}}`),
		"bad time":            withReactions(when, `{"reactionType":"👍","createdDateTime":"yesterday","user":{"user":{"id":"`+testUser+`"}}}`),
		"time in the future":  withReactions(when, `{"reactionType":"👍","createdDateTime":"2999-01-01T00:00:00Z","user":{"user":{"id":"`+testUser+`"}}}`),
		"not an array":        importBody(when, `,"reactions":{"reactionType":"👍"}`),
	} {
		resp, body := h.post(msgs, token, body)
		if resp.StatusCode != 400 || graphCode(body) != "BadRequest" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, body)
		}
		if name != "not an array" && !strings.Contains(strings.ToLower(fmt.Sprint(body)), "reaction") {
			t.Errorf("%s: the error does not say it is about a reaction: %v", name, body)
		}
	}
	if n := len(h.s.Messages(testTeam, testChannel)); n != 0 {
		t.Fatalf("rejected imports stored %d messages", n)
	}

	// Accepted: Unicode emoji (a sequence, a keycap, a flag, a skin tone),
	// the legacy names, a custom reaction with its URL, an explicit null
	// for the members that must stay unset, and no createdDateTime.
	accepted := []string{
		reaction("👍", testUser), reaction("👍🏽", testUser2), reaction("❤️", testUser), reaction("1️⃣", testUser),
		reaction("🇨🇭", testUser), reaction("👩‍💻", testUser), reaction("©️", testUser), reaction("like", testUser2), reaction("surprised", testUser),
		`{"reactionType":"custom","reactionContentUrl":"https://graph.microsoft.com/v1.0/chats/x/messages/1/hostedContents/a/$value","user":{"user":{"id":"` + testUser + `"}}}`,
		`{"reactionType":"🎉","user":{"application":null,"device":null,"user":{"id":"` + strings.ToUpper(testUser) + `"}}}`,
	}
	resp, body := h.post(msgs, token, withReactions(when, accepted...))
	if resp.StatusCode != 201 {
		t.Fatalf("import with reactions: %d %v", resp.StatusCode, body)
	}
	got := body["reactions"].([]any)
	if len(got) != len(accepted) {
		t.Fatalf("response has %d reactions, want %d", len(got), len(accepted))
	}
	first := got[0].(map[string]any)
	who := first["user"].(map[string]any)
	if first["reactionType"] != "👍" || first["createdDateTime"] != when || who["application"] != nil || who["device"] != nil ||
		who["user"].(map[string]any)["id"] != testUser || who["user"].(map[string]any)["userIdentityType"] != "aadUser" {
		t.Errorf("first reaction: %v", first)
	}
	// The user's ID is returned as the directory has it, and a reaction
	// without a time gets the time of the import.
	last := got[len(got)-1].(map[string]any)
	if last["user"].(map[string]any)["user"].(map[string]any)["id"] != testUser || last["createdDateTime"] == nil || last["createdDateTime"] == when {
		t.Errorf("last reaction: %v", last)
	}

	// Read back: by ID, in the list, and through the inspection API.
	id := body["id"].(string)
	_, one := h.do("GET", msgs+"/"+id, token, nil)
	if n := len(one["reactions"].([]any)); n != len(accepted) {
		t.Errorf("GET message: %d reactions", n)
	}
	_, list := h.do("GET", msgs, token, nil)
	if n := len(list["value"].([]any)[0].(map[string]any)["reactions"].([]any)); n != len(accepted) {
		t.Errorf("list messages: %d reactions", n)
	}
	view := h.s.Messages(testTeam, testChannel)[0]
	if len(view.Reactions) != len(accepted) || view.Reactions[1] != (ReactionView{Type: "👍🏽", UserID: testUser2, CreatedAt: view.Reactions[1].CreatedAt}) {
		t.Errorf("view: %+v", view.Reactions)
	}

	// Exactly the limit is in scope. A message without reactions has an
	// empty collection, never null.
	if resp, body := h.post(msgs, token, withReactions("2019-02-05T00:00:00Z", many(MaxImportReactions)...)); resp.StatusCode != 201 {
		t.Errorf("import with %d reactions: %d %v", MaxImportReactions, resp.StatusCode, body)
	}
	resp, body = h.post(msgs+"/"+id+"/replies", token, importBody("2019-02-06T00:00:00Z", `,"reactions":[]`))
	if r, ok := body["reactions"].([]any); resp.StatusCode != 201 || !ok || len(r) != 0 {
		t.Errorf("reply without reactions: %d %v", resp.StatusCode, body["reactions"])
	}
	if v := h.s.Messages(testTeam, testChannel); len(v) != 3 || v[2].Reactions == nil || len(v[2].Reactions) != 0 {
		t.Errorf("views: %d messages", len(v))
	}
}

func TestImportReactionsRefused(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithReactionsRefused())
	token := h.token()
	msgs := chanPath + "/messages"
	if resp, _ := h.post(chanPath+"/startMigration", token, `{"conversationCreationDateTime":"2015-01-01T00:00:00Z"}`); resp.StatusCode != 204 {
		t.Fatalf("startMigration: %d", resp.StatusCode)
	}
	const when = "2019-02-04T19:58:15.511Z"
	resp, body := h.post(msgs, token, withReactions(when, reaction("👍", testUser)))
	if resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(body), "Reactions cannot be set") {
		t.Errorf("import with reactions: %d %v", resp.StatusCode, body)
	}
	if resp, body := h.post(msgs, token, importBody(when, `,"reactions":[]`)); resp.StatusCode != 201 {
		t.Errorf("import without reactions: %d %v", resp.StatusCode, body)
	}
}

func TestValidReactionType(t *testing.T) {
	for s, want := range map[string]bool{
		"👍": true, "👍🏿": true, "❤️": true, "#️⃣": true, "🏴󠁧󠁢󠁳󠁣󠁴󠁿": true, "👨‍👩‍👧‍👦": true, "™️": true,
		"like": true, "angry": true, "sad": true, "laugh": true, "heart": true, "surprised": true, "custom": true,
		"": false, "Like": false, "thumbsup": false, ":tada:": false, "1": false, "#": false, "é": false, "👍\n": false, " 👍": false,
		"\xff": false, "👍a": false, "‍": false,
	} {
		if got := validReactionType(s); got != want {
			t.Errorf("validReactionType(%q) = %t, want %t", s, got, want)
		}
	}
}

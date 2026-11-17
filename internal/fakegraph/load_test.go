package fakegraph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// The documented limits: 5 a second into a channel, and then 429 with
// Retry-After and nothing stored, until time has passed.
func TestDocumentedLimits(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithDocumentedLimits())
	token := h.token()
	if resp, _ := h.post(chanPath+"/startMigration", token, `{"conversationCreationDateTime":"2015-01-01T00:00:00Z"}`); resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	at := func(i int) string { return fmt.Sprintf("2019-01-01T00:00:%02d.%03dZ", i/1000, i%1000) }
	accepted := 0
	for i := range 8 {
		resp, _ := h.post(chanPath+"/messages", token, importBody(at(i), ""))
		switch resp.StatusCode {
		case 201:
			accepted++
		case 429:
			if resp.Header.Get("Retry-After") != "1" {
				t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
			}
		default:
			t.Fatalf("message %d: %d", i, resp.StatusCode)
		}
	}
	if accepted != DocumentedChannelRPS {
		t.Errorf("%d messages accepted at once, want %d", accepted, DocumentedChannelRPS)
	}
	h.advance(time.Second)
	for i := range DocumentedChannelRPS {
		if resp, _ := h.post(chanPath+"/messages", token, importBody(at(100+i), "")); resp.StatusCode != 201 {
			t.Errorf("a second later, message %d: %d", i, resp.StatusCode)
		}
	}
	sum := h.s.Summary()
	if sum.Stats.ImportsAccepted != 2*DocumentedChannelRPS || sum.Stats.Throttles != 3 {
		t.Errorf("stats = %+v", sum.Stats)
	}
	// The summary counts the channel without carrying its messages.
	var found bool
	for _, c := range sum.Channels {
		if c.ChannelID == testChannel {
			found = true
			if c.Imported != 2*DocumentedChannelRPS || c.DistinctMillis != c.Messages || c.MigrationMode != migrationInProgress {
				t.Errorf("channel summary = %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("the summary lacks the channel: %+v", sum.Channels)
	}
	resp, err := http.Get(h.srv.URL + "/_fake/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var served Summary
	if err := json.NewDecoder(resp.Body).Decode(&served); err != nil || served.Stats.ImportsAccepted != sum.Stats.ImportsAccepted {
		t.Errorf("GET /_fake/summary: %v %+v", err, served.Stats)
	}
}

// The tenant-wide limit holds across channels: with a generous limit per
// channel, the app still gets only so many imports a second in all.
func TestTenantImportLimit(t *testing.T) {
	h := newHarness(t, DefaultSeed(), WithChannelRateLimit(100, 100), WithImportRateLimits(3, 0))
	token := h.token()
	if resp, _ := h.post(chanPath+"/startMigration", token, `{"conversationCreationDateTime":"2015-01-01T00:00:00Z"}`); resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	codes := map[int]int{}
	for i := range 6 {
		resp, _ := h.post(chanPath+"/messages", token, importBody(fmt.Sprintf("2019-01-01T00:00:00.%03dZ", i), ""))
		codes[resp.StatusCode]++
	}
	if codes[201] != 3 || codes[429] != 3 {
		t.Errorf("responses = %v, want 3 accepted and 3 throttled", codes)
	}
	// Reads and other writes are not imports.
	if resp, _ := h.do("GET", chanPath+"/messages", token, nil); resp.StatusCode != 200 {
		t.Errorf("read while throttled: %d", resp.StatusCode)
	}
}

// Latency is added to Graph requests and not to the control endpoints, and
// it is off unless asked for.
func TestLatency(t *testing.T) {
	var mu sync.Mutex
	var slept []time.Duration
	sleep := WithSleep(func(_ context.Context, d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() })
	h := newHarness(t, DefaultSeed(), sleep, WithLatency(20*time.Millisecond, 10*time.Millisecond))
	token := h.token()
	for range 20 {
		if resp, _ := h.do("GET", "/v1.0/organization", token, nil); resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
	}
	if resp, err := http.Get(h.srv.URL + "/_fake/summary"); err != nil || resp.StatusCode != 200 {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 21 { // the token request and 20 reads
		t.Fatalf("%d delays", len(slept))
	}
	varied := false
	for _, d := range slept {
		if d < 20*time.Millisecond || d >= 30*time.Millisecond {
			t.Errorf("a delay of %v", d)
		}
		varied = varied || d != slept[0]
	}
	if !varied {
		t.Error("the jitter added nothing")
	}

	slept = nil
	mu.Unlock()
	plain := newHarness(t, DefaultSeed(), sleep)
	plain.do("GET", "/v1.0/organization", plain.token(), nil)
	mu.Lock()
	if len(slept) != 0 {
		t.Errorf("a server without the option delayed %v", slept)
	}
}

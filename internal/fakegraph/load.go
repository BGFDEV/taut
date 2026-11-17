package fakegraph

import (
	"math/rand/v2"
	"net/http"
	"time"
)

// Modes for load tests. None is on by default: a Server without these
// options answers at once and limits nothing but what its other options
// say.

// The limits Microsoft documents for importing messages, which are also the
// numbers the egress proxy paces by: per channel and per chat, and for one
// app in one tenant across all channels and across all chats.
const (
	DocumentedChannelRPS       = 5
	DocumentedChatRPS          = 1
	DocumentedTenantChannelRPS = 50
	DocumentedTenantChatRPS    = 20
)

// WithLatency delays every Graph and sign-in request by fixed plus a
// uniformly random part of jitter before it is handled, the way a service
// on the other side of a network answers. Control endpoints are not
// delayed.
func WithLatency(fixed, jitter time.Duration) Option {
	return func(c *config) { c.latency, c.jitter = max(fixed, 0), max(jitter, 0) }
}

// WithChatRateLimit gives chats a per-chat token bucket of their own for
// message imports. Without it a chat is limited like a channel
// (WithChannelRateLimit), as before.
func WithChatRateLimit(rps float64, burst int) Option {
	return func(c *config) { c.chatRPS, c.chatBurst = rps, burst }
}

// WithImportRateLimits limits message imports across a tenant, per app:
// channelRPS for all channels together and chatRPS for all chats together.
// Zero leaves that side unlimited. A request over a limit is answered 429
// with Retry-After, and nothing is stored.
func WithImportRateLimits(channelRPS, chatRPS float64) Option {
	return func(c *config) { c.tenantChannelRPS, c.tenantChatRPS = channelRPS, chatRPS }
}

// WithDocumentedLimits enforces the documented import limits: 5 messages a
// second per channel, 1 per chat, and per app and tenant 50 a second into
// channels and 20 into chats.
func WithDocumentedLimits() Option {
	return func(c *config) {
		WithChannelRateLimit(DocumentedChannelRPS, DocumentedChannelRPS)(c)
		WithChatRateLimit(DocumentedChatRPS, DocumentedChatRPS)(c)
		WithImportRateLimits(DocumentedTenantChannelRPS, DocumentedTenantChatRPS)(c)
	}
}

// delay is how long this request waits before it is handled.
func (c *config) delay() time.Duration {
	d := c.latency
	if c.jitter > 0 {
		d += rand.N(c.jitter) //nolint:gosec // a delay, not a secret
	}
	return d
}

// importAllowed applies the limits on one message import: the
// conversation's own bucket, then the tenant's for its kind. It reports how
// long to wait when one of them is empty.
func (s *Server) importAllowed(convKey string, chat bool, tenantID, appID string) (bool, time.Duration) {
	now := s.cfg.now()
	rps, burst, tenantRPS, kind := s.cfg.channelRPS, s.cfg.channelBurst, s.cfg.tenantChannelRPS, "channels"
	if chat {
		tenantRPS, kind = s.cfg.tenantChatRPS, "chats"
		if s.cfg.chatRPS > 0 {
			rps, burst = s.cfg.chatRPS, s.cfg.chatBurst
		}
	}
	if rps > 0 {
		if ok, wait := s.limiter.allow(convKey, rps, max(burst, 1), now); !ok {
			return false, wait
		}
	}
	if tenantRPS > 0 {
		if ok, wait := s.limiter.allow("imports/"+kind+"/"+tenantID+"/"+appID, tenantRPS, max(int(tenantRPS), 1), now); !ok {
			return false, wait
		}
	}
	return true, 0
}

// ConversationSummary counts what one channel or chat holds, without its
// messages.
type ConversationSummary struct {
	// TeamID and ChannelID name a channel; ChatID a chat.
	TeamID    string `json:"teamId,omitempty"`
	ChannelID string `json:"channelId,omitempty"`
	ChatID    string `json:"chatId,omitempty"`
	Name      string `json:"name,omitempty"`
	// MigrationMode is "", "inProgress" or "completed".
	MigrationMode string `json:"migrationMode,omitempty"`
	// Messages is every message held, Imported those an app imported and
	// Replies the imported ones that are replies.
	Messages int `json:"messages"`
	Imported int `json:"imported"`
	Replies  int `json:"replies"`
	// DistinctMillis is how many different creation times, to the
	// millisecond, the messages have. It equals Messages: Graph holds one
	// message per millisecond per conversation, and so does the fake.
	DistinctMillis int `json:"distinctMillis"`
	Files          int `json:"files"`
}

// Summary is what GET /_fake/summary serves: the counters and one line per
// conversation. It is the state of a tenant after a large import in a form
// that can still be read; GET /_fake/state carries every message.
type Summary struct {
	Stats    Stats                 `json:"stats"`
	Teams    int                   `json:"teams"`
	Channels []ConversationSummary `json:"channels"`
	Chats    []ConversationSummary `json:"chats"`
}

func summarize(c *channelState) ConversationSummary {
	out := ConversationSummary{MigrationMode: c.migrationMode, Messages: len(c.messages), DistinctMillis: len(c.byMillis)}
	for _, m := range c.messages {
		if m.importedBy != "" {
			out.Imported++
			if m.replyToID != "" {
				out.Replies++
			}
		}
	}
	return out
}

// Summary returns the counters and the size of every conversation.
func (s *Server) Summary() Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Summary{Stats: s.Stats(), Teams: len(s.st.teams), Channels: []ConversationSummary{}, Chats: []ConversationSummary{}}
	for _, t := range s.st.teams {
		for _, ch := range t.channels {
			c := summarize(ch)
			c.TeamID, c.ChannelID, c.Name, c.Files = t.ID, ch.ID, ch.DisplayName, len(fileViews(ch))
			out.Channels = append(out.Channels, c)
		}
	}
	for _, chat := range s.st.chats {
		c := summarize(chat.conv)
		c.ChatID = chat.id
		out.Chats = append(out.Chats, c)
	}
	return out
}

func (s *Server) summaryHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Summary())
}

func (s *Server) tenantID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.tenantID
}

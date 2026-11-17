package fakegraph

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Action is what a fault rule does to a matching request.
type Action string

// Fault actions.
const (
	// ActionStatus answers with Fault.Status and a Graph-style error body.
	ActionStatus Action = "status"
	// ActionThrottle answers 429 with Retry-After: Fault.RetryAfterSeconds.
	ActionThrottle Action = "throttle"
	// ActionLatency delays the request by Fault.LatencyMS, then lets it
	// continue to later rules and the real handler.
	ActionLatency Action = "latency"
	// ActionDrop closes the connection without a response.
	ActionDrop Action = "drop"
	// ActionFailAfterCommit runs the real handler and discards its response:
	// the write happened but the client cannot know. With Fault.Status zero
	// the connection is closed without a response; with a Status (503, say)
	// that error is returned instead. Either way the client cannot tell
	// whether the handler accepted or rejected the request.
	ActionFailAfterCommit Action = "fail_after_commit"
)

// Fault is a rule that makes matching requests misbehave. Rules are checked
// in the order they were added; the first matching terminal rule (anything
// but latency) decides the response.
type Fault struct {
	// ID is assigned by Add when empty.
	ID string `json:"id,omitempty"`
	// Method matches the HTTP method; empty or "*" matches all.
	Method string `json:"method,omitempty"`
	// Path is a pattern matched segment by segment against the request
	// path: "*" matches within one segment (as path.Match), and a final
	// "**" matches any remainder. Empty matches every path. Examples:
	// "/v1.0/users", "/v1.0/teams/*/channels", "/v1.0/**", "/*/oauth2/v2.0/token".
	Path   string `json:"path,omitempty"`
	Action Action `json:"action"`
	// Status is the response code for ActionStatus, and optionally for
	// ActionFailAfterCommit (zero there means: drop the connection).
	Status int `json:"status,omitempty"`
	// RetryAfterSeconds is sent as Retry-After by ActionThrottle, and by
	// ActionStatus when positive.
	RetryAfterSeconds int `json:"retryAfterSeconds,omitempty"`
	// LatencyMS is the delay for ActionLatency.
	LatencyMS int `json:"latencyMs,omitempty"`
	// Count limits how many times the rule fires; zero means no limit.
	Count int `json:"count,omitempty"`
	// Probability in (0,1] fires the rule on that fraction of matches,
	// drawn from the server's seeded generator; zero means always.
	Probability float64 `json:"probability,omitempty"`
}

// FaultStatus is a rule plus how often it has fired.
type FaultStatus struct {
	Fault
	// Fired is how many requests the rule has affected.
	Fired int `json:"fired"`
	// Exhausted is true once a Count-limited rule has used up its count.
	Exhausted bool `json:"exhausted"`
}

// Faults is the server's fault-injection rule set. It is safe for
// concurrent use.
type Faults struct {
	mu    sync.Mutex
	rules []*FaultStatus
	seq   int
	rng   *rand.Rand
}

func newFaults(seed uint64) *Faults {
	return &Faults{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // determinism, not security
}

// Add validates and installs a rule, returning its ID.
func (f *Faults) Add(rule Fault) (string, error) {
	switch rule.Action {
	case ActionStatus:
		if rule.Status < 400 || rule.Status > 599 {
			return "", errors.New("fakegraph: status fault needs a status between 400 and 599")
		}
	case ActionThrottle:
		if rule.RetryAfterSeconds < 0 {
			return "", errors.New("fakegraph: retryAfterSeconds must not be negative")
		}
	case ActionLatency:
		if rule.LatencyMS <= 0 {
			return "", errors.New("fakegraph: latency fault needs latencyMs > 0")
		}
	case ActionFailAfterCommit:
		if rule.Status != 0 && (rule.Status < 400 || rule.Status > 599) {
			return "", errors.New("fakegraph: fail_after_commit status must be 0 (drop) or between 400 and 599")
		}
	case ActionDrop:
	default:
		return "", fmt.Errorf("fakegraph: unknown fault action %q", rule.Action)
	}
	if rule.Count < 0 || rule.Probability < 0 || rule.Probability > 1 {
		return "", errors.New("fakegraph: count must be >= 0 and probability within [0,1]")
	}
	for _, seg := range strings.Split(rule.Path, "/") {
		if _, err := path.Match(seg, ""); err != nil {
			return "", fmt.Errorf("fakegraph: bad path pattern %q: %w", rule.Path, err)
		}
	}
	rule.Method = strings.ToUpper(strings.TrimSpace(rule.Method))

	f.mu.Lock()
	defer f.mu.Unlock()
	if rule.ID == "" {
		f.seq++
		rule.ID = "fault-" + strconv.Itoa(f.seq)
	}
	f.rules = append(f.rules, &FaultStatus{Fault: rule})
	return rule.ID, nil
}

// MustAdd is Add for tests; it panics on an invalid rule.
func (f *Faults) MustAdd(rule Fault) string {
	id, err := f.Add(rule)
	if err != nil {
		panic(err)
	}
	return id
}

// Remove deletes a rule by ID and reports whether it existed.
func (f *Faults) Remove(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.rules {
		if r.ID == id {
			f.rules = append(f.rules[:i], f.rules[i+1:]...)
			return true
		}
	}
	return false
}

// Clear deletes every rule.
func (f *Faults) Clear() {
	f.mu.Lock()
	f.rules = nil
	f.mu.Unlock()
}

// List returns the rules, in evaluation order, with their counters.
func (f *Faults) List() []FaultStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FaultStatus, len(f.rules))
	for i, r := range f.rules {
		out[i] = *r
	}
	return out
}

// decide picks the faults for one request: the total latency to add and,
// optionally, the terminal rule that replaces or sabotages the response.
func (f *Faults) decide(r *http.Request) (latency time.Duration, terminal *Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rule := range f.rules {
		if rule.Exhausted || !rule.matches(r) {
			continue
		}
		if rule.Probability > 0 && rule.Probability < 1 && f.rng.Float64() >= rule.Probability {
			continue
		}
		rule.Fired++
		if rule.Count > 0 && rule.Fired >= rule.Count {
			rule.Exhausted = true
		}
		if rule.Action == ActionLatency {
			latency += time.Duration(rule.LatencyMS) * time.Millisecond
			continue
		}
		picked := rule.Fault
		return latency, &picked
	}
	return latency, nil
}

func (f *FaultStatus) matches(r *http.Request) bool {
	if f.Method != "" && f.Method != "*" && f.Method != r.Method {
		return false
	}
	return matchPath(f.Path, r.URL.Path)
}

// matchPath implements the pattern language documented on Fault.Path.
func matchPath(pattern, p string) bool {
	if pattern == "" {
		return true
	}
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, pat := range ps {
		if pat == "**" && i == len(ps)-1 {
			return true
		}
		if i >= len(segs) {
			return false
		}
		if ok, _ := path.Match(pat, segs[i]); !ok {
			return false
		}
	}
	return len(ps) == len(segs)
}

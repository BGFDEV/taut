// Package budget shares a tenant-wide request rate between egress proxies.
//
// Microsoft Graph, SharePoint and Slack throttle per tenant (or workspace),
// not per process, so the egress proxies of one migration must split a single
// budget. The control plane (the controller) runs an Allocator; each egress proxy asks
// it for a Lease ("you may send RPS requests per second for the next TTL")
// and asks again every TTL/2 while it has traffic for that key.
//
// # Fairness and the overshoot bound
//
// A Grant returns total / activeHolders, where a holder is active if it
// asked within the last 2×TTL. The allocator never revokes: a lease already
// handed out stays valid until its holder next refreshes. Consequently:
//
//   - With stable membership every holder converges on total/n within one
//     refresh (TTL/2), and the sum of leases equals the total exactly.
//   - When k holders join within one refresh interval of an existing set of
//     n, the holders already there keep total/n until they refresh, and the
//     joiners receive total/(n+1) … total/(n+k). The sum of unexpired leases
//     is therefore at most total × (1 + 1/(n+1) + … + 1/(n+k)). The worst
//     case is a cold start (n = 0): total × H(k), e.g. 1.5× for two
//     egress proxies and about 2.3× for five. The excess lasts at most one TTL
//     (in practice TTL/2, the refresh interval), after which the sum is the
//     total again.
//   - When a holder stops asking, the others keep their smaller shares until
//     it has been silent for 2×TTL. That errs toward under-use, never
//     over-use.
//
// # Untrusted callers
//
// Lease requests arrive over the bus, which does not authenticate them. The
// allocator therefore bounds what a caller can make it remember: tenant,
// scope and holder names longer than MaxNameLen are refused; at most
// DefaultMaxKeys budgets are tracked, and a request for one more is answered
// with the UnknownScopeRPS minimum and not recorded; holders and keys that
// have been silent for 2×TTL are swept out as Grants arrive. These limits
// only bound the damage. The real control is NATS subject permissions: only
// egress proxies may request on taut.budget.lease and only the control plane may
// answer.
//
// The transient overshoot is deliberate: it keeps the allocator stateless
// about outstanding leases and lets an egress proxy start working at once. The
// upstream's own 429 handling (see the egress proxy package) absorbs it.
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

// Scopes with a configured budget.
const (
	ScopeGraph      = "graph"
	ScopeSharePoint = "sharepoint"
	ScopeSlack      = "slack"
)

// Subject is the bus subject lease requests are served on.
const Subject = wire.SubjectBudgetLease

const (
	// DefaultTTL is how long a lease is valid when none is configured.
	DefaultTTL = 10 * time.Second
	// UnknownScopeRPS is the total budget of a scope with no configured
	// total: small enough to be safe against any upstream.
	UnknownScopeRPS = 1.0
	// remoteTimeout bounds one lease request over the bus.
	remoteTimeout = 2 * time.Second
	// DefaultMaxKeys is how many budgets an Allocator tracks at once.
	DefaultMaxKeys = 1024
	// MaxNameLen bounds the tenant, scope and holder strings, in bytes.
	MaxNameLen = 256
	// maxHoldersPerKey bounds the allocator's memory against a caller that
	// invents holder names. Beyond it the share stops shrinking.
	maxHoldersPerKey = 4096
)

// DefaultTotals returns the default total requests per second per scope.
func DefaultTotals() map[string]float64 {
	return map[string]float64{ScopeGraph: 40, ScopeSharePoint: 20, ScopeSlack: 1}
}

// Key names one shared budget.
type Key struct {
	// Tenant is the Entra tenant ID, or the upstream host for traffic
	// that has no tenant.
	Tenant string `json:"tenant"`
	// Scope is ScopeGraph, ScopeSharePoint or ScopeSlack.
	Scope string `json:"scope"`
}

// Lease is a holder's share of a budget.
type Lease struct {
	// RPS is the rate the holder may send at.
	RPS float64
	// TTL is how long the lease is valid. Holders refresh at TTL/2.
	TTL time.Duration
}

// Source is where an egress proxy gets its leases.
type Source interface {
	// Lease returns holder's current share of key's budget.
	Lease(ctx context.Context, key Key, holder string) (Lease, error)
}

// --- allocator ------------------------------------------------------------------

// Allocator divides each budget equally among the holders that are asking
// for it. It is safe for concurrent use.
type Allocator struct {
	totals map[string]float64
	ttl    time.Duration
	now    func() time.Time

	mu        sync.Mutex
	holders   map[Key]map[string]time.Time // key → holder → last asked
	maxKeys   int
	lastSweep time.Time
}

// NewAllocator returns an Allocator. totals maps scope to total requests per
// second; scopes missing from it take DefaultTotals, and a nil map means all
// defaults. ttl defaults to DefaultTTL and now to time.Now.
func NewAllocator(totals map[string]float64, ttl time.Duration, now func() time.Time) *Allocator {
	merged := DefaultTotals()
	for scope, rps := range totals {
		if rps > 0 {
			merged[scope] = rps
		}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Allocator{totals: merged, ttl: ttl, now: now, holders: map[Key]map[string]time.Time{}, maxKeys: DefaultMaxKeys}
}

func (a *Allocator) total(scope string) float64 {
	if rps, ok := a.totals[scope]; ok {
		return rps
	}
	return UnknownScopeRPS
}

// prune drops holders of key that have been silent for more than 2×TTL.
// The caller holds a.mu.
func (a *Allocator) prune(key Key, now time.Time) map[string]time.Time {
	hs := a.holders[key]
	for h, last := range hs {
		if now.Sub(last) > 2*a.ttl {
			delete(hs, h)
		}
	}
	if len(hs) == 0 {
		delete(a.holders, key)
		return nil
	}
	return hs
}

// sweep prunes every key. The caller holds a.mu.
func (a *Allocator) sweep(now time.Time) {
	for key := range a.holders {
		a.prune(key, now)
	}
	a.lastSweep = now
}

func validNames(key Key, holder string) bool {
	return len(key.Tenant) <= MaxNameLen && len(key.Scope) <= MaxNameLen && len(holder) <= MaxNameLen
}

// Grant records holder as active for key and returns its equal share.
//
// A request the allocator will not keep state for (a name longer than
// MaxNameLen, or a new key when DefaultMaxKeys are already tracked) gets
// the UnknownScopeRPS minimum and is not recorded.
func (a *Allocator) Grant(key Key, holder string) Lease {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	minimum := Lease{RPS: UnknownScopeRPS, TTL: a.ttl}
	if !validNames(key, holder) {
		return minimum
	}
	// Amortized cleanup: once per TTL, drop everything that went silent,
	// so abandoned keys do not accumulate.
	if now.Sub(a.lastSweep) >= a.ttl {
		a.sweep(now)
	}
	hs := a.prune(key, now)
	if hs == nil {
		if len(a.holders) >= a.maxKeys {
			a.sweep(now)
			if len(a.holders) >= a.maxKeys {
				return minimum
			}
		}
		hs = map[string]time.Time{}
		a.holders[key] = hs
	}
	if _, known := hs[holder]; known || len(hs) < maxHoldersPerKey {
		hs[holder] = now
	}
	return Lease{RPS: a.total(key.Scope) / float64(len(hs)), TTL: a.ttl}
}

// KeyStatus describes one budget for a status display.
type KeyStatus struct {
	Key      Key      `json:"key"`
	TotalRPS float64  `json:"totalRps"`
	ShareRPS float64  `json:"shareRps"`
	Holders  []string `json:"holders"`
}

// Snapshot returns the budgets that currently have active holders, sorted by
// tenant and scope. It contains no secrets.
func (a *Allocator) Snapshot() []KeyStatus {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]KeyStatus, 0, len(a.holders))
	for key := range a.holders {
		hs := a.prune(key, now)
		if hs == nil {
			continue
		}
		names := make([]string, 0, len(hs))
		for h := range hs {
			names = append(names, h)
		}
		sort.Strings(names)
		total := a.total(key.Scope)
		out = append(out, KeyStatus{Key: key, TotalRPS: total, ShareRPS: total / float64(len(names)), Holders: names})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Tenant != out[j].Key.Tenant {
			return out[i].Key.Tenant < out[j].Key.Tenant
		}
		return out[i].Key.Scope < out[j].Key.Scope
	})
	return out
}

// --- bus protocol ---------------------------------------------------------------

type leaseRequest struct {
	Tenant string `json:"tenant"`
	Scope  string `json:"scope"`
	Holder string `json:"holder"`
}

type leaseReply struct {
	RPS   float64 `json:"rps,omitempty"`
	TTLMs int64   `json:"ttlMs,omitempty"`
	Error string  `json:"error,omitempty"`
}

// Serve answers lease requests on Subject until the returned function is
// called.
func (a *Allocator) Serve(b bus.Bus) (stop func(), err error) {
	return b.Serve(Subject, func(data []byte) []byte {
		var req leaseRequest
		var rep leaseReply
		switch err := json.Unmarshal(data, &req); {
		case err != nil:
			rep.Error = "the lease request is not valid JSON"
		case req.Tenant == "" || req.Scope == "" || req.Holder == "":
			rep.Error = "the lease request needs tenant, scope and holder"
		case !validNames(Key{Tenant: req.Tenant, Scope: req.Scope}, req.Holder):
			rep.Error = "the lease request's tenant, scope or holder is too long"
		default:
			l := a.Grant(Key{Tenant: req.Tenant, Scope: req.Scope}, req.Holder)
			rep = leaseReply{RPS: l.RPS, TTLMs: l.TTL.Milliseconds()}
		}
		out, _ := json.Marshal(rep)
		return out
	})
}

// --- sources --------------------------------------------------------------------

type static struct{ rps map[string]float64 }

// Static returns a Source that hands every caller the configured rate for
// the key's scope, undivided. It is for single-process runs and tests, where
// one egress proxy owns the whole budget. Scopes missing from rps take
// DefaultTotals; scopes unknown to both get UnknownScopeRPS.
func Static(rps map[string]float64) Source {
	merged := DefaultTotals()
	for scope, v := range rps {
		if v > 0 {
			merged[scope] = v
		}
	}
	return static{merged}
}

func (s static) Lease(_ context.Context, key Key, _ string) (Lease, error) {
	rps, ok := s.rps[key.Scope]
	if !ok {
		rps = UnknownScopeRPS
	}
	return Lease{RPS: rps, TTL: DefaultTTL}, nil
}

type remote struct{ b bus.Bus }

// Remote returns a Source that asks the Allocator served on b. Each request
// has a two-second timeout. When nobody answers, the error wraps
// bus.ErrNoResponder.
func Remote(b bus.Bus) Source { return remote{b} }

func (r remote) Lease(ctx context.Context, key Key, holder string) (Lease, error) {
	data, _ := json.Marshal(leaseRequest{Tenant: key.Tenant, Scope: key.Scope, Holder: holder})
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	out, err := r.b.Request(ctx, Subject, data)
	if err != nil {
		return Lease{}, fmt.Errorf("budget: requesting a lease for %s/%s: %w", key.Tenant, key.Scope, err)
	}
	var rep leaseReply
	if err := json.Unmarshal(out, &rep); err != nil {
		return Lease{}, fmt.Errorf("budget: the lease reply for %s/%s is not valid JSON", key.Tenant, key.Scope)
	}
	if rep.Error != "" {
		return Lease{}, errors.New("budget: the control plane refused the lease: " + rep.Error)
	}
	if rep.RPS <= 0 || rep.TTLMs <= 0 {
		return Lease{}, fmt.Errorf("budget: the lease reply for %s/%s has no usable rate", key.Tenant, key.Scope)
	}
	return Lease{RPS: rep.RPS, TTL: time.Duration(rep.TTLMs) * time.Millisecond}, nil
}

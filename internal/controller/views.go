package controller

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"bytes"
	"encoding/json"

	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/store"
)

// DashboardView is everything `taut dashboard` shows, in one response, so the
// dashboard is drawn from a single consistent snapshot.
type DashboardView struct {
	// Run carries the run and its Progress (always set).
	Run     RunView     `json:"run"`
	Cluster ClusterView `json:"cluster"`
	// At is the controller's clock when the snapshot was taken. Rates are computed
	// against it, not the client's clock.
	At time.Time `json:"at"`
}

// ClusterView is the state of the workers and sidecars.
type ClusterView struct {
	// Workers holding leases in this run.
	Workers []store.Holder `json:"workers"`
	// Budgets are the shared rate budgets that currently have egress proxies
	// asking for a share. Each holder is one egress proxy.
	Budgets []budget.KeyStatus `json:"budgets"`
	// Throttles are the most recent throttle events from the egress proxies,
	// newest first (at most 50). The controller keeps them in memory only.
	Throttles []ThrottleView `json:"throttles"`
}

// ThrottleView is one occasion on which Microsoft or Slack told an egress proxy
// to slow down.
type ThrottleView struct {
	Tenant string `json:"tenant"`
	// Scope is the budget scope: "graph", "sharepoint" or "slack".
	Scope string `json:"scope"`
	// Holder is the egress proxy that was throttled.
	Holder string    `json:"holder"`
	At     time.Time `json:"at"`
	// Until is when the tenant-wide pause it caused ends.
	Until time.Time `json:"until"`
}

// LogPage is one page of an audit log query.
type LogPage struct {
	Entries []store.LogEntry `json:"entries"`
	// Next is the cursor for the following page; nil on the last page.
	Next *store.LogCursor `json:"next,omitempty"`
}

// FinalizeRequest asks the controller to finalize a run.
type FinalizeRequest struct {
	// Channels limits finalizing to these Slack conversation IDs: channels
	// and direct messages alike. Empty means everything in the run.
	Channels []string `json:"channels,omitempty"`
	// Retain keeps staging after the run is finalized.
	Retain bool `json:"retain,omitempty"`
}

// FinalizeView is the answer to a finalize request.
type FinalizeView struct {
	Run RunView `json:"run"`
	// Channels is how many channels the request selected and Chats how
	// many direct-message conversations; Already how many of both were
	// completed before it.
	Channels int `json:"channels"`
	Chats    int `json:"chats"`
	Already  int `json:"already"`
	// Releasing is how many chats are being taken out of migration mode
	// without the run being finalized: the answer for a run that failed
	// or was cancelled, whose chats would otherwise stay in it.
	Releasing int `json:"releasing,omitempty"`
	Teams     int `json:"teams"`
}

// Dashboard returns the dashboard snapshot for a run.
func (c *Client) Dashboard(ctx context.Context, id string) (DashboardView, error) {
	var v DashboardView
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/dashboard", nil, nil, &v)
	return v, err
}

// AuditLog returns one page of a run's audit trail.
func (c *Client) AuditLog(ctx context.Context, id string, f store.LogFilter) (LogPage, error) {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("stream", f.Stream)
	set("op", f.Op)
	set("outcome", f.Outcome)
	set("q", f.Contains)
	if !f.Since.IsZero() {
		q.Set("since", f.Since.UTC().Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		q.Set("until", f.Until.UTC().Format(time.RFC3339Nano))
	}
	if f.After != (store.LogCursor{}) {
		q.Set("afterStream", f.After.Stream)
		q.Set("afterSeq", strconv.FormatInt(f.After.Seq, 10))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var v LogPage
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/audit?"+q.Encode(), nil, nil, &v)
	return v, err
}

// Finalize asks the controller to finalize a run's channels.
func (c *Client) Finalize(ctx context.Context, id string, req FinalizeRequest) (FinalizeView, error) {
	body, _ := json.Marshal(req)
	var v FinalizeView
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/finalize", bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"}, &v)
	return v, err
}

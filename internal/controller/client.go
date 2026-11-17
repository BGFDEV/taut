package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/ui/sanitize"
)

// Client talks to a controller server.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for the controller at baseURL. token may be empty.
func NewClient(baseURL, token string) *Client {
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		token: token,
		// No overall timeout: export uploads are long. Each call is bounded
		// by its context instead.
		http: &http.Client{},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, hdr map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fail.Wrap(err, "controller.bad_url", "The controller URL is not valid", "", "Check controller.url in the config file.")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		why := err.Error()
		if errors.As(err, &ue) {
			why = ue.Err.Error()
		}
		return fail.Wrap(err, "controller.unreachable", "Could not reach the controller at "+c.base, why,
			"Start the control plane (`make up` for the local stack), or correct controller.url in the config file. `taut doctor` checks the connection.")
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var env apiError
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(data, &env) == nil && env.Error != nil && env.Error.What != "" {
			e := env.Error
			// The envelope can quote what Microsoft Graph or the bus said.
			e.Code, e.What = sanitize.Text(e.Code), sanitize.Text(e.What)
			e.Why, e.Next = sanitize.Lines(e.Why), sanitize.Lines(e.Next)
			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				e.Exit = fail.ExitPermission
			case http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadRequest:
				e.Exit = fail.ExitUsage
			}
			return e
		}
		return fail.New("controller.error", "The controller returned an unexpected response",
			sanitize.Text(resp.Status), "Check the controller's logs (`make logs`), then retry.")
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fail.Wrap(err, "controller.bad_response", "The controller returned a response taut could not read", "",
			"Make sure the taut CLI and the controller are the same version.")
	}
	sanitizeView(out)
	return nil
}

// sanitizeView makes a decoded view safe to draw. A view carries text the controller
// only relays: channel and team names from the Slack export, errors quoting
// Microsoft Graph, and holder, tenant and scope names that arrived over the
// unauthenticated bus. The client is where that text enters the CLI, so it
// is cleaned here, once, for every call that returns a view (Run,
// LatestRun, CreateRun, UploadExport, Wait, Dashboard and Finalize), rather than
// wherever it is rendered.
//
// A LogPage is deliberately left alone: an audit log export must hold entries
// exactly as recorded, so whatever draws an entry sanitizes it.
func sanitizeView(out any) {
	switch v := out.(type) {
	case *RunView:
		sanitizeRun(v)
	case *DashboardView:
		sanitizeRun(&v.Run)
		sanitizeCluster(&v.Cluster)
	case *FinalizeView:
		sanitizeRun(&v.Run)
	}
}

func sanitizeRun(v *RunView) {
	v.ID, v.Name = sanitize.Text(v.ID), sanitize.Text(v.Name)
	v.Status = store.RunStatus(sanitize.Text(string(v.Status)))
	v.ConfigHash, v.ExportSHA256 = sanitize.Text(v.ConfigHash), sanitize.Text(v.ExportSHA256)
	v.BaseRunID = sanitize.Text(v.BaseRunID)
	for i := range v.Passes {
		v.Passes[i].ID = sanitize.Text(v.Passes[i].ID)
		v.Passes[i].Status = store.RunStatus(sanitize.Text(string(v.Passes[i].Status)))
	}
	p := v.Progress
	if p == nil {
		return
	}
	for i := range p.Stages {
		p.Stages[i].Stage = sanitize.Text(p.Stages[i].Stage)
	}
	for i := range p.Channels {
		c := &p.Channels[i]
		c.SlackID, c.SlackName = sanitize.Text(c.SlackID), sanitize.Text(c.SlackName)
		c.TargetName, c.Team, c.Kind = sanitize.Text(c.TargetName), sanitize.Text(c.Team), sanitize.Text(c.Kind)
		c.State = store.ChannelState(sanitize.Text(string(c.State)))
		// Errors keep their line breaks; the drill-in shows them whole.
		c.LastError = sanitize.Lines(c.LastError)
	}
	for i := range p.Problems {
		l := &p.Problems[i]
		l.LaneID, l.Stage, l.Status = sanitize.Text(l.LaneID), sanitize.Text(l.Stage), sanitize.Text(l.Status)
		l.Error = sanitize.Lines(l.Error)
	}
}

func sanitizeCluster(f *ClusterView) {
	for i := range f.Workers {
		h := &f.Workers[i]
		h.Holder = sanitize.Text(h.Holder)
		for j := range h.Stages {
			h.Stages[j] = sanitize.Text(h.Stages[j])
		}
	}
	for i := range f.Budgets {
		b := &f.Budgets[i]
		b.Key.Tenant, b.Key.Scope = sanitize.Text(b.Key.Tenant), sanitize.Text(b.Key.Scope)
		for j := range b.Holders {
			b.Holders[j] = sanitize.Text(b.Holders[j])
		}
	}
	for i := range f.Throttles {
		t := &f.Throttles[i]
		t.Tenant, t.Scope, t.Holder = sanitize.Text(t.Tenant), sanitize.Text(t.Scope), sanitize.Text(t.Holder)
	}
}

// CreateRun starts a run for the config file, or returns the active run with
// the same name (RunView.Resumed is then true). With replace, an active run
// is cancelled first.
func (c *Client) CreateRun(ctx context.Context, configYAML []byte, replace bool) (RunView, error) {
	body, _ := json.Marshal(map[string]any{"config": string(configYAML), "replace": replace})
	var v RunView
	err := c.do(ctx, http.MethodPost, "/v1/runs", bytes.NewReader(body), map[string]string{"Content-Type": "application/json"}, &v)
	return v, err
}

// Run returns a run with its progress.
func (c *Client) Run(ctx context.Context, id string) (RunView, error) {
	var v RunView
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, nil, &v)
	return v, err
}

// LatestRun returns the most recent run for a config file name.
func (c *Client) LatestRun(ctx context.Context, name string) (RunView, error) {
	var v RunView
	err := c.do(ctx, http.MethodGet, "/v1/runs?name="+url.QueryEscape(name), nil, nil, &v)
	return v, err
}

// UploadExport streams the export ZIP. sha256 is the hex digest of the
// file, which the controller verifies on arrival.
func (c *Client) UploadExport(ctx context.Context, id string, r io.Reader, sha256 string) (RunView, error) {
	var v RunView
	err := c.do(ctx, http.MethodPut, "/v1/runs/"+url.PathEscape(id)+"/export", r,
		map[string]string{"Content-Type": "application/zip", "X-Taut-SHA256": sha256}, &v)
	return v, err
}

// UploadOverrides sends the identity override CSV.
func (c *Client) UploadOverrides(ctx context.Context, id string, r io.Reader) error {
	return c.do(ctx, http.MethodPut, "/v1/runs/"+url.PathEscape(id)+"/overrides", r, map[string]string{"Content-Type": "text/csv"}, nil)
}

// Retry returns failed lanes to the queue and reports how many.
func (c *Client) Retry(ctx context.Context, id string) (int, error) {
	var out struct{ Lanes int }
	err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/retry", nil, nil, &out)
	return out.Lanes, err
}

// Cancel stops a run.
func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/cancel", nil, nil, nil)
}

// VerifyAuditLog asks the controller to recompute the run's audit hash chains.
func (c *Client) VerifyAuditLog(ctx context.Context, id string) (ok bool, problem string, err error) {
	var out struct {
		OK      bool   `json:"ok"`
		Problem string `json:"problem"`
	}
	err = c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/audit/verify", nil, nil, &out)
	// The problem names a stream and may quote an entry; callers print it.
	return out.OK, sanitize.Text(out.Problem), err
}

// Wait polls a run until fn returns false or the run reaches a state that
// cannot change on its own (completed or cancelled). It returns the last
// view. A run that has failed keeps being reported, since an operator may
// retry it; fn decides whether to keep watching.
func (c *Client) Wait(ctx context.Context, id string, every time.Duration, fn func(RunView) bool) (RunView, error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		v, err := c.Run(ctx, id)
		if err != nil {
			return v, err
		}
		if !fn(v) || v.Status == store.RunCompleted || v.Status == store.RunCancelled || v.Status == store.RunFinalized {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-t.C:
		}
	}
}

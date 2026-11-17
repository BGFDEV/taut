package controller

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
)

// Incremental passes.
//
// A migration can be imported in several passes: the first imports an
// export, and each later one imports, from a newer export of the same
// workspace, what the passes before it did not. A pass is a run that names
// the run it follows. What it takes over from that run, and when it may,
// is the plan stage's business (worker.loadBase); what is decided here is
// which run a new pass follows, and that it can be started at all.

// LedgerView is what a run recorded as being in Teams, as the API reports
// it to the preflight of the pass that would follow.
type LedgerView struct {
	store.Ledger
	// TenantID is the Entra tenant the run wrote to.
	TenantID string `json:"tenantId,omitempty"`
}

// LedgerKey is one line of the keys listing: a Slack message that has a
// post in Teams.
type LedgerKey struct {
	TS string `json:"ts"`
	// Edited is Slack's "edited" timestamp of the message when it was
	// imported ("" when it had not been edited); nil when not recorded.
	Edited *string `json:"edited"`
}

// passBase finds the run a new incremental pass would follow: the
// migration's latest run that finished importing. Passes that were
// withdrawn before they were planned do not count; they took nothing.
//
// The error, when there is one, is ready to return to the caller.
func (s *Server) passBase(ctx context.Context, name string) (store.Run, error) {
	latest, ok, err := s.svc.Store.LatestRun(ctx, name)
	if err != nil {
		return store.Run{}, err
	}
	if !ok {
		return store.Run{}, fail.New("run.nothing_to_follow", "There is no earlier pass to add to",
			"No migration named "+name+" has been run here.",
			"Run `taut migrate` for the first import. An incremental pass adds to one that has finished.").WithExit(fail.ExitUsage)
	}
	for latest.Status == store.RunCancelled && latest.Incremental() {
		l, err := s.svc.Store.Ledger(ctx, latest.ID)
		if err != nil {
			return store.Run{}, err
		}
		if l.Planned {
			break
		}
		if latest, err = s.svc.Store.GetRun(ctx, latest.BaseRunID); err != nil {
			return store.Run{}, err
		}
	}
	switch latest.Status {
	case store.RunCompleted, store.RunFinalized:
		return latest, nil
	case store.RunFinalizing:
		return store.Run{}, fail.New("run.base_finalizing", "The earlier pass is being finalized",
			"Run "+latest.ID+" is completing migration mode and adding members.",
			"Run `taut finalize` to watch it finish, then `taut migrate --incremental` again.").WithExit(fail.ExitUsage)
	case store.RunCancelled:
		return store.Run{}, fail.New("run.base_abandoned", "The last run of this migration was abandoned part-way",
			"Run "+latest.ID+" was cancelled after it had started importing. Its ledger may lack messages that reached Teams, so a pass cannot safely start from it.",
			"Run `taut migrate --new` to import from the start. Messages already in Teams are recognised by their timestamps and not imported twice, provided the export and the config file are the ones that run used.").WithExit(fail.ExitUsage)
	}
	return store.Run{}, fail.New("run.base_not_finished", "The earlier pass has not finished importing",
		"Run "+latest.ID+" is "+string(latest.Status)+".",
		"Run `taut migrate` to finish it. An incremental pass can only follow a pass that is complete.").WithExit(fail.ExitUsage)
}

// firstImportActive is the refusal of a pass while the migration's first
// run is still active: importing, stopped part-way, or being finalized.
func firstImportActive(existing store.Run) *fail.Error {
	if existing.Status == store.RunFinalizing {
		return fail.New("run.base_finalizing", "The earlier pass is being finalized",
			"Run "+existing.ID+" is completing migration mode and adding members.",
			"Run `taut finalize` to watch it finish, then `taut migrate --incremental` again.").WithExit(fail.ExitUsage)
	}
	return fail.New("run.base_not_finished", "The first import of this migration has not finished",
		"Run "+existing.ID+" is "+string(existing.Status)+".",
		"Run `taut migrate` to finish it. An incremental pass can only follow a pass that is complete.").WithExit(fail.ExitUsage)
}

// tenantOf returns the Entra tenant a run's config names.
func tenantOf(run store.Run) string {
	if m, err := config.Parse(run.Config); err == nil {
		return m.Target.Entra.TenantID
	}
	return ""
}

// createPass starts an incremental pass, or returns the one in progress.
//
// An incremental pass in progress is resumed, like any run. It can be
// replaced by another only while it has not been planned: until then it has
// carried nothing over and written nothing. Once planned it holds the
// migration's ledger, and replacing it would start the next pass from a
// ledger that may lack what it had already sent.
func (s *Server) createPass(w http.ResponseWriter, r *http.Request, m *config.Config, configYAML, hash string, replace bool) {
	ctx := r.Context()
	name := m.Metadata.Name
	if existing, ok, err := s.svc.Store.ActiveRun(ctx, name); err != nil {
		s.internalError(w, err)
		return
	} else if ok {
		if !existing.Incremental() {
			writeError(w, http.StatusConflict, firstImportActive(existing))
			return
		}
		switch {
		case replace:
			l, err := s.svc.Store.Ledger(ctx, existing.ID)
			if err != nil {
				s.internalError(w, err)
				return
			}
			if l.Planned || existing.Status == store.RunFinalizing {
				writeError(w, http.StatusConflict, fail.New("run.pass_started",
					fmt.Sprintf("Pass %d has started importing and cannot be replaced", existing.Pass),
					"Run "+existing.ID+" holds the record of what the migration has put into Teams, including what this pass has sent so far.",
					"Run `taut migrate` to resume it; it never imports a message twice. A newer export goes into the pass after it.").WithExit(fail.ExitUsage))
				return
			}
			if err := s.svc.Store.CancelRun(ctx, existing.ID); err != nil {
				s.internalError(w, err)
				return
			}
			s.log.Info("unplanned pass withdrawn", "run", existing.ID, "name", existing.Name, "pass", existing.Pass)
		case existing.ConfigHash != hash:
			writeError(w, http.StatusConflict, fail.New("run.config_changed",
				"The config file has changed since this pass started",
				fmt.Sprintf("Pass %d (run %s) is still in progress with the earlier config file.", existing.Pass, existing.ID),
				"Restore the config file and run `taut migrate` to resume the pass. If it has not started importing, `taut migrate --incremental --new` starts it again with the config file as it is now.").WithExit(fail.ExitUsage))
			return
		default:
			v := view(existing)
			v.Resumed = true
			writeJSON(w, http.StatusOK, v)
			return
		}
	}

	base, err := s.passBase(ctx, name)
	if err != nil {
		var fe *fail.Error
		if errors.As(err, &fe) && fe.Exit == fail.ExitUsage {
			writeError(w, http.StatusConflict, fe)
			return
		}
		s.internalError(w, err)
		return
	}
	if was := tenantOf(base); was != "" && !strings.EqualFold(was, m.Target.Entra.TenantID) {
		writeError(w, http.StatusConflict, fail.New("run.tenant_changed",
			"The config file names a different tenant from the earlier passes",
			"Run "+base.ID+" imported into tenant "+was+"; the config file now names "+m.Target.Entra.TenantID+".",
			"Restore target.entra.tenantId. An incremental pass adds to the teams the earlier passes created, which exist in that tenant only.").WithExit(fail.ExitUsage))
		return
	}

	dek, err := staging.NewDataKey()
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer dek.Destroy()
	wrapped, err := s.svc.KEK.Wrap(dek)
	if err != nil {
		s.internalError(w, err)
		return
	}
	run := store.Run{
		ID: store.NewRunID(), Name: name, Config: []byte(configYAML), ConfigHash: hash,
		Status: store.RunAwaitingExport, WrappedKey: wrapped, BaseRunID: base.ID,
		// The pass's ledger holds the chats of the passes before it,
		// whether or not its own config migrates direct messages.
		// The layout also tells a worker that predates passes to leave the
		// run alone.
		Layout: pipeline.LayoutForPass(m.Target.DirectMessages.Enabled || pipeline.LayoutHasChats(base.Layout)),
	}
	switch err := s.svc.Store.CreateRun(ctx, run); {
	case errors.Is(err, store.ErrActiveRunExists), errors.Is(err, store.ErrBaseSuperseded):
		writeError(w, http.StatusConflict, fail.New("run.exists", "A pass of this migration was just started elsewhere", "",
			"Run `taut migrate` again to attach to it."))
		return
	case errors.Is(err, store.ErrBaseNotFinished), errors.Is(err, store.ErrNotFound):
		// The base changed between the check and the insert.
		writeError(w, http.StatusConflict, fail.New("run.base_not_finished", "The earlier pass changed while this one was being started", err.Error(),
			"Run `taut status`, then `taut migrate --incremental` again.").WithExit(fail.ExitUsage))
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	created, err := s.svc.Store.GetRun(ctx, run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.log.Info("incremental pass created", "run", run.ID, "name", run.Name, "pass", created.Pass, "base", base.ID)
	writeJSON(w, http.StatusCreated, view(created))
}

// nextPass answers with the ledger a new incremental pass of the named
// migration would start from, or with the reason there cannot be one now.
// It is what `taut preflight --incremental` compares the export against,
// and it applies the rules createPass does, so the preflight cannot promise
// a pass the controller would then refuse.
func (s *Server) nextPass(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, fail.Usagef("Pass ?name=<config name>.", "A migration name is required"))
		return
	}
	ctx := r.Context()
	// With replace=1 the question is asked for `taut migrate --incremental
	// --new`: what would the pass follow if the one in progress, which has
	// not been planned, were withdrawn first.
	replace := r.URL.Query().Get("replace") == "1"
	var base store.Run
	existing, active, err := s.svc.Store.ActiveRun(ctx, name)
	switch {
	case err != nil:
		s.internalError(w, err)
		return
	case active && !existing.Incremental():
		writeError(w, http.StatusConflict, firstImportActive(existing))
		return
	case active && !replace:
		writeError(w, http.StatusConflict, fail.New("run.pass_in_progress", fmt.Sprintf("Pass %d of this migration is in progress", existing.Pass),
			"Run "+existing.ID+" is "+string(existing.Status)+".",
			"Run `taut migrate` to resume it, or `taut status` to see where it stands. The next pass can be checked once this one has finished.").WithExit(fail.ExitUsage))
		return
	case active:
		l, err := s.svc.Store.Ledger(ctx, existing.ID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if l.Planned || existing.Status == store.RunFinalizing {
			writeError(w, http.StatusConflict, fail.New("run.pass_started",
				fmt.Sprintf("Pass %d has started importing and cannot be replaced", existing.Pass),
				"Run "+existing.ID+" holds the record of what the migration has put into Teams, including what this pass has sent so far.",
				"Run `taut migrate` to resume it; it never imports a message twice. A newer export goes into the pass after it.").WithExit(fail.ExitUsage))
			return
		}
		if base, err = s.svc.Store.GetRun(ctx, existing.BaseRunID); err != nil {
			s.internalError(w, err)
			return
		}
	default:
		base, err = s.passBase(ctx, name)
	}
	if err != nil {
		var fe *fail.Error
		if errors.As(err, &fe) && fe.Exit == fail.ExitUsage {
			writeError(w, http.StatusConflict, fe)
			return
		}
		s.internalError(w, err)
		return
	}
	l, err := s.svc.Store.Ledger(ctx, base.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, LedgerView{Ledger: l, TenantID: tenantOf(base)})
}

// ledger answers with what a run recorded as being in Teams.
func (s *Server) ledger(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	l, err := s.svc.Store.Ledger(r.Context(), run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, LedgerView{Ledger: l, TenantID: tenantOf(run)})
}

// ledgerKeys streams the Slack timestamps of one conversation's messages
// that are in Teams, one JSON object per line, in timestamp order. A large
// channel has millions; nothing is buffered, and the stream is compressed
// for a client that accepts it.
func (s *Server) ledgerKeys(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		writeError(w, http.StatusBadRequest, fail.Usagef("Pass ?channel=<Slack conversation ID>.", "A conversation ID is required"))
		return
	}
	if _, err := s.svc.Store.Channel(r.Context(), run.ID, channel); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, notFound("conversation "+channel+" in this run"))
			return
		}
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	out := bufio.NewWriter(w)
	var zw *gzip.Writer
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		zw = gzip.NewWriter(w)
		out = bufio.NewWriter(zw)
	}
	enc := json.NewEncoder(out)
	err := s.svc.Store.PostedKeys(r.Context(), run.ID, channel, func(ts string, edited *string) error {
		return enc.Encode(LedgerKey{TS: ts, Edited: edited})
	})
	if err != nil {
		// The status line is gone. An unterminated stream is how the
		// client learns of it: the final line below is then missing.
		s.log.Warn("listing ledger keys failed", "run", run.ID, "error", err)
		return
	}
	// A last line that is not a key, so that a truncated stream cannot be
	// taken for a complete one.
	_, _ = out.WriteString(ledgerKeysEnd + "\n")
	_ = out.Flush()
	if zw != nil {
		_ = zw.Close()
	}
}

// ledgerKeysEnd terminates a complete keys listing.
const ledgerKeysEnd = `{"end":true}`

// CreateOptions say how a run is started.
type CreateOptions struct {
	// Replace cancels an active run with the same name first.
	Replace bool
	// Incremental starts a pass that follows the migration's last finished
	// run.
	Incremental bool
}

// CreateRunWith starts a run for the config file, or returns the active run
// with the same name (RunView.Resumed is then true).
func (c *Client) CreateRunWith(ctx context.Context, configYAML []byte, o CreateOptions) (RunView, error) {
	body, _ := json.Marshal(map[string]any{"config": string(configYAML), "replace": o.Replace, "incremental": o.Incremental})
	var v RunView
	err := c.do(ctx, http.MethodPost, "/v1/runs", strings.NewReader(string(body)), map[string]string{"Content-Type": "application/json"}, &v)
	return v, err
}

// NextPass returns the ledger a new incremental pass of the named migration
// would start from. The error says why there cannot be one now: nothing to
// follow, or a run still in progress.
//
// With replace the question is asked for a pass that would first withdraw
// the one in progress, which only a pass that has not been planned allows.
func (c *Client) NextPass(ctx context.Context, name string, replace bool) (LedgerView, error) {
	path := "/v1/passes/next?name=" + url.QueryEscape(name)
	if replace {
		path += "&replace=1"
	}
	var v LedgerView
	err := c.do(ctx, http.MethodGet, path, nil, nil, &v)
	return v, err
}

// Ledger returns what a run recorded as being in Teams.
func (c *Client) Ledger(ctx context.Context, id string) (LedgerView, error) {
	var v LedgerView
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/ledger", nil, nil, &v)
	return v, err
}

// LedgerKeys calls fn for every Slack message of a conversation that the
// run recorded as being in Teams, in timestamp order.
func (c *Client) LedgerKeys(ctx context.Context, id, channel string, fn func(slackTS string, edited *string) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/v1/runs/"+url.PathEscape(id)+"/ledger/keys?channel="+url.QueryEscape(channel), nil)
	if err != nil {
		return fail.Wrap(err, "controller.bad_url", "The controller URL is not valid", "", "Check controller.url in the config file.")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail.Wrap(err, "controller.unreachable", "Could not reach the controller at "+c.base, err.Error(),
			"Start the control plane, or correct controller.url in the config file. `taut doctor` checks the connection.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Let the shared path read the error envelope.
		return c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/ledger/keys?channel="+url.QueryEscape(channel), nil, nil, nil)
	}
	incomplete := fail.New("controller.bad_response", "The controller's list of imported messages was cut short",
		"The connection closed before the list was complete.", "Run the command again.")
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	ended := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if string(line) == ledgerKeysEnd {
			ended = true
			break
		}
		var k LedgerKey
		if err := json.Unmarshal(line, &k); err != nil || k.TS == "" {
			return incomplete
		}
		if err := fn(k.TS, k.Edited); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil || !ended {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return incomplete
	}
	return nil
}

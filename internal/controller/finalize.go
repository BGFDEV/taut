package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/wire"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/worker"
)

// maxThrottles bounds the throttle events kept for the dashboard;
// maxBudgetRows and maxBudgetHolders what one snapshot reports.
const (
	maxThrottles     = 50
	maxBudgetRows    = 32
	maxBudgetHolders = 64
)

// looksLikeTenantID reports whether s has the shape of an Entra tenant ID
// (a GUID), as opposed to a host name.
func looksLikeTenantID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F'):
			return false
		}
	}
	return true
}

// dashboard answers with everything the dashboard shows.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	v := DashboardView{Run: view(run), At: time.Now().UTC()}
	p, err := s.svc.Store.Progress(r.Context(), run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	v.Run.Progress = &p
	hs, err := s.svc.Store.Holders(r.Context(), run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	// Budgets and throttles come from the bus, which authenticates
	// nobody. Show only this run's tenant, and bound everything: the
	// dashboard asks every second.
	tenant := ""
	if m, err := config.Parse(run.Config); err == nil {
		tenant = m.Target.Entra.TenantID
	}
	mine := func(t string) bool { return t == tenant || !looksLikeTenantID(t) }
	v.Cluster = ClusterView{Workers: hs, Budgets: []budget.KeyStatus{}, Throttles: []ThrottleView{}}
	if v.Cluster.Workers == nil {
		v.Cluster.Workers = []store.Holder{}
	}
	for _, t := range s.recentThrottles() {
		if mine(t.Tenant) {
			v.Cluster.Throttles = append(v.Cluster.Throttles, t)
		}
	}
	if s.svc.Budgets != nil {
		for _, b := range s.svc.Budgets() {
			if !mine(b.Key.Tenant) || len(v.Cluster.Budgets) == maxBudgetRows {
				continue
			}
			switch b.Key.Scope {
			case budget.ScopeGraph, budget.ScopeSharePoint, budget.ScopeSlack:
			default:
				b.Key.Scope = "other"
			}
			b.Key.Tenant = clipText(b.Key.Tenant, 64)
			// The dashboard needs how many egress proxies share the budget,
			// not what they call themselves.
			names := make([]string, min(len(b.Holders), maxBudgetHolders))
			for i := range names {
				names[i] = clipText(b.Holders[i], 64)
			}
			b.Holders = names
			v.Cluster.Budgets = append(v.Cluster.Budgets, b)
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) recentThrottles() []ThrottleView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ThrottleView{}, s.throttles...)
}

// listenThrottles remembers the throttle events egress proxies publish, for the
// dashboard. Nothing authenticates the sender, so an event is only ever
// displayed, and only if its timestamp is plausible.
func (s *Server) listenThrottles(ctx context.Context) {
	if s.svc.Bus == nil {
		return
	}
	events, stop := s.svc.Bus.Listen(wire.SubjectThrottle)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-events:
			if !ok {
				return
			}
			var ev wire.Throttle
			if json.Unmarshal(data, &ev) != nil || ev.Tenant == "" || ev.RetryAfterMs < 0 {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, ev.At)
			if age := time.Since(at); err != nil || age > time.Minute || age < -time.Minute {
				continue
			}
			pause := min(time.Duration(ev.RetryAfterMs)*time.Millisecond, 10*time.Minute)
			tv := ThrottleView{
				Tenant: clipText(ev.Tenant, 64), Scope: clipText(ev.Scope, 32), Holder: clipText(ev.Holder, 128),
				At: at.UTC(), Until: at.Add(pause).UTC(),
			}
			s.mu.Lock()
			s.throttles = append([]ThrottleView{tv}, s.throttles...)
			if len(s.throttles) > maxThrottles {
				s.throttles = s.throttles[:maxThrottles]
			}
			s.mu.Unlock()
		}
	}
}

// clipText bounds a string that arrived over the bus and drops control
// characters, since it ends up on an operator's terminal.
func clipText(v string, n int) string {
	out := make([]rune, 0, len(v))
	for _, r := range v {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if len(out) == n {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// auditLog returns a filtered page of the audit trail.
func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	bad := func(field, why string) {
		writeError(w, http.StatusBadRequest, fail.New("audit.bad_filter",
			"The audit log filter is not valid", field+": "+why,
			"Run `taut audit --help` for the filters.").WithExit(fail.ExitUsage))
	}
	f := store.LogFilter{Stream: q.Get("stream"), Op: q.Get("op"), Outcome: q.Get("outcome"), Contains: q.Get("q")}
	for name, v := range map[string]string{"stream": f.Stream, "op": f.Op, "q": f.Contains, "afterStream": q.Get("afterStream")} {
		if len(v) > 256 || strings.ContainsRune(v, 0) {
			bad(name, "must be at most 256 bytes of text")
			return
		}
	}
	if f.Outcome != "" && f.Outcome != "ok" && f.Outcome != "error" {
		bad("outcome", `must be "ok" or "error"`)
		return
	}
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				bad(name, "must be an RFC 3339 time")
				return
			}
			*dst = t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			bad("limit", "must be a positive number")
			return
		}
		f.Limit = n
	}
	// A cursor is both halves or neither.
	if as, aq := q.Get("afterStream"), q.Get("afterSeq"); as != "" || aq != "" {
		seq, err := strconv.ParseInt(aq, 10, 64)
		if err != nil || seq < 0 || as == "" {
			bad("afterStream, afterSeq", "must be given together, as returned in the previous page's next cursor")
			return
		}
		f.After = store.LogCursor{Stream: as, Seq: seq}
	}
	entries, next, err := s.svc.Store.LogQuery(r.Context(), run.ID, f)
	if err != nil {
		s.internalError(w, err)
		return
	}
	page := LogPage{Entries: entries}
	if page.Entries == nil {
		page.Entries = []store.LogEntry{}
	}
	if next != (store.LogCursor{}) {
		page.Next = &next
	}
	writeJSON(w, http.StatusOK, page)
}

// finalizeRun starts finalizing a run: completing migration mode on its
// channels and adding each team's members. Workers do the work; this
// only queues it. Repeating the request is harmless.
func (s *Server) finalizeRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	var req FinalizeRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err == nil && len(body) > 0 {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, fail.New("finalize.bad_request", "The finalize request is not valid JSON", "",
			"Send {\"channels\": [...], \"retain\": false}, or use `taut finalize`.").WithExit(fail.ExitUsage))
		return
	}
	ctx := r.Context()
	s.finalizing.Lock()
	defer s.finalizing.Unlock()
	// Read the run again now that no other finalize request can interleave.
	if run, err = s.svc.Store.GetRun(ctx, run.ID); err != nil {
		s.internalError(w, err)
		return
	}
	channels, err := s.svc.Store.Channels(ctx, run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	respond := func(sel worker.FinalizeSelection) {
		run, err := s.svc.Store.GetRun(ctx, run.ID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		out := FinalizeView{Run: view(run), Channels: sel.Channels, Chats: sel.Chats, Already: sel.Already, Teams: sel.Teams}
		if p, err := s.svc.Store.Progress(ctx, run.ID); err == nil {
			out.Run.Progress = &p
		}
		writeJSON(w, http.StatusOK, out)
	}
	notReady := func(run store.Run) {
		why := "The run is " + string(run.Status) + "."
		next := "Wait for the import to finish (`taut status`), then run `taut finalize` again."
		switch run.Status {
		case store.RunFailed:
			next = "Some of the import failed. Run `taut status` to see what, `taut migrate` to resume it, then `taut finalize`."
		case store.RunCancelled:
			next = "This run was cancelled. Run `taut migrate` to start the migration again."
		}
		writeError(w, http.StatusConflict, fail.New("finalize.not_ready",
			"The migration is not ready to be finalized", why, next).WithExit(fail.ExitBlockers))
	}
	switch run.Status {
	case store.RunCompleted, store.RunFinalizing:
	case store.RunFailed, store.RunCancelled:
		// The import will not be finished as it stands, so there is
		// nothing to finalize. But the run's chats are live, in front of
		// their participants, in migration mode; those can be had back.
		// Only those: a request that names anything else, or nothing, is
		// answered as before.
		if !onlyChats(channels, req.Channels) {
			notReady(run)
			return
		}
		n, err := s.svc.Store.ReleaseChats(ctx, run.ID, req.Channels)
		switch {
		case errors.Is(err, store.ErrPurged):
			writeError(w, http.StatusConflict, fail.New("finalize.purged", "This run's staging and key are gone",
				"Run "+run.ID+" was purged; its chats were taken out of migration mode before that.", "Nothing to do.").WithExit(fail.ExitUsage))
			return
		case err != nil:
			s.internalError(w, err)
			return
		}
		s.log.Info("releasing chats", "run", run.ID, "status", string(run.Status), "chats", n)
		s.wake(ctx)
		out := FinalizeView{Run: view(run), Chats: len(req.Channels), Releasing: n}
		if p, err := s.svc.Store.Progress(ctx, run.ID); err == nil {
			out.Run.Progress = &p
		}
		writeJSON(w, http.StatusOK, out)
		return
	case store.RunFinalized:
		// Everything is already finalized. Say so rather than fail: the
		// operator's intent is satisfied.
		respond(allFinalized(channels))
		return
	default:
		notReady(run)
		return
	}
	teams, err := s.svc.Store.Teams(ctx, run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	lanes, err := s.svc.Store.Lanes(ctx, run.ID, store.StageFinalize)
	if err != nil {
		s.internalError(w, err)
		return
	}
	inProgress := map[string]bool{}
	for _, l := range lanes {
		inProgress[l.Key] = true
	}
	sel, err := worker.PlanFinalize(channels, teams, req.Channels, inProgress, req.Retain)
	if err != nil {
		writeError(w, http.StatusBadRequest, fail.New("finalize.unknown_channel",
			"A channel in the request is not part of this run", err.Error(),
			"Run `taut status` to list the run's channels, and name them by Slack name or ID.").WithExit(fail.ExitUsage))
		return
	}
	if err := s.svc.Store.BeginFinalize(ctx, run.ID, sel.Plan); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, notFound("run"))
		case errors.Is(err, store.ErrNotFinalizable):
			// The run changed state between the check and the write.
			now, gerr := s.svc.Store.GetRun(ctx, run.ID)
			switch {
			case gerr != nil:
				s.internalError(w, gerr)
			case now.Status == store.RunFinalized:
				respond(allFinalized(channels))
			default:
				notReady(now)
			}
		default:
			s.internalError(w, err)
		}
		return
	}
	s.log.Info("finalize requested", "run", run.ID, "channels", sel.Channels, "chats", sel.Chats, "already", sel.Already,
		"teams", sel.Teams, "retain", req.Retain)
	s.wake(ctx)
	respond(sel)
}

// onlyChats reports whether ids names at least one conversation and every
// one of them is a direct message of the run.
func onlyChats(channels []store.ChannelMapping, ids []string) bool {
	chat := map[string]bool{}
	for _, c := range channels {
		chat[c.SlackID] = c.IsChat()
	}
	for _, id := range ids {
		if !chat[id] {
			return false
		}
	}
	return len(ids) > 0
}

// allFinalized is the answer for a run in which everything is finalized
// already.
func allFinalized(channels []store.ChannelMapping) worker.FinalizeSelection {
	sel := worker.FinalizeSelection{Already: len(channels)}
	for _, c := range channels {
		if c.IsChat() {
			sel.Chats++
		} else {
			sel.Channels++
		}
	}
	return sel
}

// purge deletes the staging of finalized runs that are not retained. Staging
// holds the Slack export and every staged message and file; once a
// migration is finalized nothing needs it, and keeping it would keep a
// second copy of the workspace's data.
//
// The objects go first and the data key last: if deleting is interrupted,
// the next pass finishes it, and only when nothing is left is the run
// marked purged and its key destroyed.
func (s *Server) purge(ctx context.Context) {
	runs, err := s.svc.Store.Purgeable(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("looking for runs to purge failed", "error", err)
		}
		return
	}
	for _, run := range runs {
		// The export is the only record of who belongs in each team. Never
		// delete it while a team is still without its members lane.
		if run.Status != store.RunFinalized {
			// A cancelled run: nothing will ever read its staging again.
		} else if ok, err := s.membersDone(ctx, run.ID); err != nil || !ok {
			s.log.Error("not purging staging: a team of this finalized run has not had its members added",
				"run", run.ID, "name", run.Name, "error", err)
			continue
		}
		n, err := s.svc.Blobs.DeletePrefix(ctx, staging.RunPrefix(run.ID))
		if err != nil {
			s.log.Warn("purging staging failed; it will be retried", "run", run.ID, "deleted", n, "error", err)
			continue
		}
		if err := s.svc.Store.MarkPurged(ctx, run.ID); err != nil {
			s.log.Warn("recording the purge failed; it will be retried", "run", run.ID, "error", err)
			continue
		}
		s.log.Info("staging purged", "run", run.ID, "name", run.Name, "objects", n)
	}
}

// membersDone reports whether every team of a run has a finished members
// lane.
func (s *Server) membersDone(ctx context.Context, runID string) (bool, error) {
	teams, err := s.svc.Store.Teams(ctx, runID)
	if err != nil {
		return false, err
	}
	lanes, err := s.svc.Store.Lanes(ctx, runID, store.StageMembers)
	if err != nil {
		return false, err
	}
	done := 0
	for _, l := range lanes {
		if l.Status == store.LaneDone {
			done++
		}
	}
	return done >= len(teams), nil
}

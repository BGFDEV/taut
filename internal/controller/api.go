package controller

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
)

// Services are what the API needs beyond configuration. Any of them may be
// nil, in which case the run endpoints answer 503 and only health and
// version are served.
type Services struct {
	Store store.Store
	// Blobs is the raw staging backend; the API encrypts on the way in.
	Blobs staging.Blobs
	KEK   staging.KEK
	Bus   bus.Bus
	// Token, when non-empty, is required as a bearer token on every /v1
	// request.
	Token string
	// AllowTestEndpoints permits config files that override the Microsoft or
	// Slack endpoints. Only a local stack with fake servers needs it; a
	// config file must otherwise not choose where requests and sign-ins go.
	AllowTestEndpoints bool
	// MaxExportBytes bounds an uploaded export. Zero means 200 GiB.
	MaxExportBytes int64
	// Budgets reports the shared rate budgets for the dashboard. Optional.
	Budgets func() []budget.KeyStatus
	// Middleware, when set, wraps the whole API handler (telemetry).
	Middleware func(http.Handler) http.Handler
}

func (s Services) ready() bool { return s.Store != nil && s.Blobs != nil && !s.KEK.IsZero() }

// maxOverrides bounds the override CSV; a few bytes per user, so 8 MiB is
// far beyond any real directory.
const maxOverrides = 8 << 20

// RunView is a run as the API reports it.
type RunView struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Status       store.RunStatus `json:"status"`
	ConfigHash   string          `json:"configHash"`
	ExportSHA256 string          `json:"exportSha256,omitempty"`
	ExportBytes  int64           `json:"exportBytes,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
	FinishedAt   *time.Time      `json:"finishedAt,omitempty"`
	// Resumed is true in the response to POST /v1/runs when an existing
	// active run was returned instead of a new one being created.
	Resumed  bool            `json:"resumed,omitempty"`
	Progress *store.Progress `json:"progress,omitempty"`
	// Retain is true when staging is kept after the run is finalized;
	// PurgedAt is when staging was purged.
	Retain   bool       `json:"retain,omitempty"`
	PurgedAt *time.Time `json:"purgedAt,omitempty"`
}

func view(r store.Run) RunView {
	return RunView{
		ID: r.ID, Name: r.Name, Status: r.Status, ConfigHash: r.ConfigHash,
		ExportSHA256: r.ExportSHA256, ExportBytes: r.ExportBytes,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, FinishedAt: r.FinishedAt,
		Retain: r.Retain, PurgedAt: r.PurgedAt,
	}
}

// apiError is the JSON error envelope, mirroring fail.Error.
type apiError struct {
	Error *fail.Error `json:"error"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, apiError{Error: fail.From(err)})
}

// internalError reports a failure inside the controller. The detail goes to the log;
// the caller gets a reference to it, not the database's or the object
// store's own words.
func (s *Server) internalError(w http.ResponseWriter, err error) {
	var fe *fail.Error
	if errors.As(err, &fe) && fe.Code != "unexpected" && fe.Code != "" {
		// Structured errors are written for operators and safe to return.
		writeError(w, http.StatusInternalServerError, fe)
		return
	}
	s.log.Error("request failed", "error", err)
	writeError(w, http.StatusInternalServerError, fail.New("controller.internal",
		"The controller could not complete the request", "An internal error occurred; the detail is in the controller's log.",
		"Check the controller's logs (`make logs` for the local stack), then retry."))
}

func notFound(what string) error {
	return fail.New("run.not_found", "No such "+what, "", "Run `taut status` to see the current run, or `taut migrate` to start one.")
}

func (s *Server) routes(mux *http.ServeMux) {
	svc := s.svc
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !svc.ready() {
				writeError(w, http.StatusServiceUnavailable, fail.New("controller.not_configured",
					"The controller is running without a database or staging",
					"TAUT_POSTGRES_URL, the staging settings or TAUT_STAGING_KEY are not set.",
					"Set them in the controller's environment and restart it."))
				return
			}
			if svc.Token != "" {
				got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				if subtle.ConstantTimeCompare([]byte(got), []byte(svc.Token)) != 1 {
					writeError(w, http.StatusUnauthorized, fail.New("controller.unauthorized",
						"The controller rejected the API token", "",
						"Set TAUT_CONTROLLER_TOKEN to the same secret the controller was started with.").WithExit(fail.ExitPermission))
					return
				}
			}
			h(w, r)
		}
	}

	mux.HandleFunc("POST /v1/runs", guard(s.createRun))
	mux.HandleFunc("GET /v1/runs", guard(s.findRun))
	mux.HandleFunc("GET /v1/runs/{id}", guard(s.getRun))
	mux.HandleFunc("PUT /v1/runs/{id}/export", guard(s.putExport))
	mux.HandleFunc("PUT /v1/runs/{id}/overrides", guard(s.putOverrides))
	mux.HandleFunc("POST /v1/runs/{id}/retry", guard(s.retryRun))
	mux.HandleFunc("POST /v1/runs/{id}/cancel", guard(s.cancelRun))
	mux.HandleFunc("GET /v1/runs/{id}/audit", guard(s.auditLog))
	mux.HandleFunc("GET /v1/runs/{id}/dashboard", guard(s.dashboard))
	mux.HandleFunc("POST /v1/runs/{id}/finalize", guard(s.finalizeRun))
	mux.HandleFunc("GET /v1/runs/{id}/audit/verify", guard(s.verifyAuditLog))
}

func (s *Server) wake(ctx context.Context) {
	if s.svc.Bus != nil {
		if err := s.svc.Bus.Wake(ctx); err != nil {
			s.log.Warn("wake-up not delivered; workers will pick the work up on their next poll", "error", err)
		}
	}
}

// createRun starts a run for a config file, or returns the active run with the
// same name so that `taut migrate` is naturally resumable.
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Config string `json:"config"`
		// Replace cancels an active run with the same name first.
		Replace bool `json:"replace"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fail.New("request.invalid", "The request body is not valid JSON", err.Error(), "Upgrade taut so the CLI and the controller match."))
		return
	}
	m, err := config.Parse([]byte(req.Config))
	if err == nil {
		err = m.Validate().Err()
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if !s.svc.AllowTestEndpoints && (m.Target.Entra.Authority != "" || m.Target.Entra.GraphBaseURL != "" || m.Source.Slack.API.BaseURL != "") {
		writeError(w, http.StatusUnprocessableEntity, fail.New("config.endpoint_override",
			"This config file overrides the Microsoft or Slack endpoints",
			"target.entra.authority, target.entra.graphBaseURL and source.slack.api.baseURL exist for testing against fake servers; this control plane does not accept them.",
			"Remove those fields from the config file. To target a sovereign cloud, set target.entra.cloud instead.").WithExit(fail.ExitUsage))
		return
	}
	sum := sha256.Sum256([]byte(req.Config))
	hash := hex.EncodeToString(sum[:])
	ctx := r.Context()

	if existing, ok, err := s.svc.Store.ActiveRun(ctx, m.Metadata.Name); err != nil {
		s.internalError(w, err)
		return
	} else if ok {
		switch {
		case req.Replace:
			if err := s.svc.Store.CancelRun(ctx, existing.ID); err != nil {
				s.internalError(w, err)
				return
			}
			s.log.Info("run superseded", "run", existing.ID, "name", existing.Name)
		case existing.ConfigHash != hash:
			writeError(w, http.StatusConflict, fail.New("run.config_changed",
				"The config file has changed since this migration started",
				"Run "+existing.ID+" is still in progress with the earlier config file.",
				"Restore the config file to resume, or run `taut migrate --new` to start over. Starting over does not remove what was already imported into Teams.").WithExit(fail.ExitUsage))
			return
		default:
			v := view(existing)
			v.Resumed = true
			writeJSON(w, http.StatusOK, v)
			return
		}
	}

	// Starting over leaves the run before this one for good, whatever
	// state it is in. Nobody will finalize it now, so its chats must not
	// stay in migration mode in front of the people in them. (A run that
	// was still active was cancelled above, which queues the same work;
	// this covers one that had finished importing.)
	if req.Replace {
		if latest, ok, err := s.svc.Store.LatestRun(ctx, m.Metadata.Name); err != nil {
			s.internalError(w, err)
			return
		} else if ok && latest.PurgedAt == nil && latest.Status != store.RunFinalized {
			if n, err := s.svc.Store.ReleaseChats(ctx, latest.ID, nil); err != nil {
				s.internalError(w, err)
				return
			} else if n > 0 {
				s.log.Info("releasing the chats of the run that is being replaced", "run", latest.ID, "chats", n)
			}
		}
		s.wake(ctx)
	}

	// A migration that finished importing is not imported again unless
	// the operator says so. A second run writes into teams that people may
	// already be using.
	if !req.Replace {
		latest, ok, err := s.svc.Store.LatestRun(ctx, m.Metadata.Name)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if ok && (latest.Status == store.RunCompleted || latest.Status == store.RunFinalized) {
			what, why, next := "This migration has already been imported",
				"Run "+latest.ID+" finished importing and is waiting to be finalized.",
				"Run `taut finalize` to finalize it, or `taut migrate --new` to import again from the start."
			if latest.Status == store.RunFinalized {
				what, why = "This migration is finished", "Run "+latest.ID+" was imported and finalized; its channels are open to their members."
				next = "Nothing more to do. To migrate again, change the config file and run `taut migrate --new`."
			}
			writeError(w, http.StatusConflict, fail.New("run.already_imported", what, why, next).WithExit(fail.ExitUsage))
			return
		}
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
		ID: store.NewRunID(), Name: m.Metadata.Name, Config: []byte(req.Config), ConfigHash: hash,
		Status: store.RunAwaitingExport, WrappedKey: wrapped,
		// What the run needs of the workers that plan and work it, decided
		// here from its config and not left to whichever worker plans it.
		Layout: pipeline.LayoutFor(m.Target.DirectMessages.Enabled),
	}
	if err := s.svc.Store.CreateRun(ctx, run); err != nil {
		if errors.Is(err, store.ErrActiveRunExists) {
			// Lost a race with another `taut migrate`; the caller can retry
			// and will resume that run.
			writeError(w, http.StatusConflict, fail.New("run.exists", "A migration with this name was just started elsewhere", "", "Run `taut migrate` again to attach to it."))
			return
		}
		s.internalError(w, err)
		return
	}
	created, err := s.svc.Store.GetRun(ctx, run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.log.Info("run created", "run", run.ID, "name", run.Name)
	writeJSON(w, http.StatusCreated, view(created))
}

func (s *Server) loadRun(w http.ResponseWriter, r *http.Request) (store.Run, bool) {
	run, err := s.svc.Store.GetRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, notFound("run"))
		return run, false
	}
	if err != nil {
		s.internalError(w, err)
		return run, false
	}
	return run, true
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	v := view(run)
	if run.Status != store.RunAwaitingExport {
		p, err := s.svc.Store.Progress(r.Context(), run.ID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		v.Progress = &p
	}
	writeJSON(w, http.StatusOK, v)
}

// findRun returns the latest run with the given name.
func (s *Server) findRun(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, fail.Usagef("Pass ?name=<config name>.", "A run name is required"))
		return
	}
	run, ok, err := s.svc.Store.LatestRun(r.Context(), name)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, notFound("run named "+name))
		return
	}
	v := view(run)
	if run.Status != store.RunAwaitingExport {
		if p, err := s.svc.Store.Progress(r.Context(), run.ID); err == nil {
			v.Progress = &p
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// runBlobs returns the encrypted staging view for a run. The caller must
// call the returned function to wipe the key.
func (s *Server) runBlobs(run store.Run) (staging.Blobs, func(), error) {
	dek, err := s.svc.KEK.Unwrap(run.WrappedKey)
	if err != nil {
		return nil, nil, err
	}
	return staging.Encrypted(s.svc.Blobs, dek), dek.Destroy, nil
}

// putExport receives the Slack export ZIP, encrypts it into staging and
// queues the run for planning. The body is streamed; it is never held in
// memory or written to the controller's disk in the clear.
func (s *Server) putExport(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	if run.Status != store.RunAwaitingExport {
		// Already uploaded: a resumed `taut migrate` may send it again.
		writeJSON(w, http.StatusOK, view(run))
		return
	}
	blobs, wipe, err := s.runBlobs(run)
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer wipe()

	limit := s.svc.MaxExportBytes
	if limit <= 0 {
		limit = 200 << 30
	}
	key := staging.ExportKey(run.ID)
	body, digest := staging.SHA256Reader(http.MaxBytesReader(w, r.Body, limit))
	n, err := blobs.Put(r.Context(), key, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	sum := digest()
	// The client states the digest it computed; a mismatch means the
	// upload was corrupted or truncated in transit.
	if want := r.Header.Get("X-Taut-SHA256"); want != "" && !strings.EqualFold(want, sum) {
		_ = blobs.Delete(r.Context(), key)
		writeError(w, http.StatusBadRequest, fail.New("export.corrupt_upload",
			"The export arrived damaged", "Its checksum does not match the file on your machine.",
			"Run `taut migrate` again to re-upload it."))
		return
	}
	if n == 0 {
		_ = blobs.Delete(r.Context(), key)
		writeError(w, http.StatusBadRequest, fail.New("export.empty", "The uploaded export is empty", "", "Check source.slack.export in the config file."))
		return
	}
	if err := s.svc.Store.SetExport(r.Context(), run.ID, key, sum, n); err != nil {
		s.internalError(w, err)
		return
	}
	s.log.Info("export stored", "run", run.ID, "bytes", n)
	s.wake(r.Context())
	updated, err := s.svc.Store.GetRun(r.Context(), run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(updated))
}

func (s *Server) putOverrides(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	if run.Status != store.RunAwaitingExport {
		// The identity map is fixed once a run is planned.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	blobs, wipe, err := s.runBlobs(run)
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer wipe()
	if _, err := blobs.Put(r.Context(), pipeline.OverridesKey(run.ID), http.MaxBytesReader(w, r.Body, maxOverrides)); err != nil {
		writeError(w, http.StatusBadRequest, fail.Wrap(err, "identity.overrides_upload", "The identity override file could not be stored", "", "Check that it is a CSV under 8 MB and retry."))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) retryRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	n, err := s.svc.Store.RetryFailed(r.Context(), run.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.log.Info("failed lanes retried", "run", run.ID, "lanes", n)
	s.wake(r.Context())
	writeJSON(w, http.StatusOK, map[string]int{"lanes": n})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	if err := s.svc.Store.CancelRun(r.Context(), run.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.log.Info("run cancelled", "run", run.ID)
	// Cancelling queues the release of the run's chats; get it worked.
	s.wake(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) verifyAuditLog(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	if err := s.svc.Store.VerifyLog(r.Context(), run.ID); err != nil {
		if !store.IsLogCorrupt(err) {
			// The check could not be made. That is not evidence of
			// tampering and must not be reported as such.
			s.internalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "problem": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// reconcileLoop keeps run statuses in step with their lanes. Workers only
// ever touch lanes; noticing that a whole run has finished, or can no longer
// progress, is the controller's job.
func (s *Server) reconcileLoop(ctx context.Context, every time.Duration) {
	if s.svc.Store == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reconcile(ctx)
		}
	}
}

// Reconcile runs one pass of the control loop: bring run statuses in step
// with their lanes, then purge the staging of runs that have been finalized.
func (s *Server) Reconcile(ctx context.Context) {
	changed, err := s.svc.Store.Reconcile(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("reconcile failed", "error", err)
		}
		return
	}
	for _, r := range changed {
		s.log.Info("run status changed", "run", r.ID, "name", r.Name, "status", string(r.Status))
	}
	s.purge(ctx)
}

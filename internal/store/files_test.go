package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/store/storetest"
)

// A database created at schema version 1 and already holding a run is
// migrated to the current version: existing lanes get the new stage ranks,
// the new stage slots in between team and load, and old rows read back
// with the new columns empty.
func TestMigrateFromV1WithRows(t *testing.T) {
	t.Parallel()
	url, schema := storetest.NewSchema(t)
	s, err := store.Open(t.Context(), url, store.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := t.Context()
	if err := store.MigrateTo(ctx, s, 1); err != nil {
		t.Fatalf("migrate to v1: %v", err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM schema_migrations`); n != 1 {
		t.Fatalf("schema at version count %d, want 1", n)
	}

	// A running run as version 1 would have stored it.
	exec(t, s, `INSERT INTO runs (id, name, config, config_hash, status, planned_at) VALUES ('r1', 'acme', '', 'h', 'running', now())`)
	exec(t, s, `INSERT INTO map_teams (run_id, idx, name) VALUES ('r1', 0, 'Engineering')`)
	exec(t, s, `INSERT INTO map_channels (run_id, slack_id, slack_name, team_index, teams_id, state) VALUES ('r1', 'C1', 'general', 0, '19:x', 'created')`)
	for _, l := range []struct{ id, stage string }{
		{"plan", "plan"}, {"extract:C1", "extract"}, {"transform:C1", "transform"}, {"team:0", "team"}, {"load:C1", "load"},
	} {
		exec(t, s, `INSERT INTO lanes (run_id, id, stage, run_created, fence, cursor) SELECT 'r1', $1, $2, created_at, 7, '{"k": 1}' FROM runs WHERE id = 'r1'`, l.id, l.stage)
		exec(t, s, `INSERT INTO batches (run_id, lane_id, seq) VALUES ('r1', $1, 0)`, l.id)
	}
	exec(t, s, `INSERT INTO map_messages (run_id, channel_id, slack_ts, part, target_ms) VALUES ('r1', 'C1', '1.1', 0, 1001)`)
	if rank := scalar[int](t, s, `SELECT stage_rank FROM lanes WHERE id = 'load:C1'`); rank != 4 {
		t.Fatalf("version 1 load rank = %d, want 4", rank)
	}
	// Version 1 does not know the files stage.
	if _, err := s.Pool().Exec(ctx, `INSERT INTO lanes (run_id, id, stage, run_created) VALUES ('r1', 'files:C1', 'files', now())`); err == nil {
		t.Fatal("version 1 accepted a files lane")
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate again: %v", err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM schema_migrations`); n != schemaVersion {
		t.Fatalf("schema_migrations has %d rows, want %d", n, schemaVersion)
	}

	// Existing lanes: new ranks, everything else intact.
	exec(t, s, `INSERT INTO lanes (run_id, id, stage, run_created) SELECT 'r1', 'files:C1', 'files', created_at FROM runs WHERE id = 'r1'`)
	exec(t, s, `INSERT INTO batches (run_id, lane_id, seq) VALUES ('r1', 'files:C1', 0)`)
	wantRank := map[string]int{"plan": 0, "extract:C1": 1, "transform:C1": 2, "team:0": 3, "files:C1": 4, "load:C1": 5}
	for lane, want := range wantRank {
		if got := scalar[int](t, s, `SELECT stage_rank FROM lanes WHERE id = $1`, lane); got != want {
			t.Errorf("lane %s: stage_rank = %d, want %d", lane, got, want)
		}
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE fence = 7 AND cursor::text = '{"k": 1}' AND status = 'open'`); n != 5 {
		t.Errorf("%d of 5 old lanes kept their fence and cursor", n)
	}
	if def := scalar[string](t, s, `SELECT indexdef FROM pg_indexes WHERE schemaname = $1 AND indexname = 'lanes_claim'`, schema); def == "" {
		t.Error("lanes_claim index missing after migration")
	}
	if _, err := s.Pool().Exec(ctx, `INSERT INTO lanes (run_id, id, stage, run_created) VALUES ('r1', 'x', 'polish', now())`); err == nil {
		t.Error("an unknown stage is accepted after migration")
	}

	// Claim order through the API: files sits between team and load.
	var got []string
	for {
		l, err := s.Claim(ctx, store.ClaimFilter{Stages: allStages}, "h", ttl)
		if err != nil {
			t.Fatal(err)
		}
		if l == nil {
			break
		}
		if l.Fence != 8 && l.LaneID != "files:C1" {
			t.Errorf("lane %s: fence %d after migration, want 8", l.LaneID, l.Fence)
		}
		got = append(got, l.LaneID)
	}
	want := []string{"plan", "extract:C1", "transform:C1", "team:0", "files:C1", "load:C1"}
	if len(got) != len(want) {
		t.Fatalf("claim order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("claim order = %v, want %v", got, want)
		}
	}

	// Old rows read back with the new columns empty, and take new data.
	c, err := s.Channel(ctx, "r1", "C1")
	if err != nil || c.TeamsID != "19:x" || c.State != store.ChannelCreated || c.FilesDriveID != "" || c.FilesFolderID != "" {
		t.Fatalf("Channel = %+v, %v", c, err)
	}
	if m, err := s.GetMessage(ctx, "r1", store.MessageKey{ChannelID: "C1", SlackTS: "1.1"}); err != nil || m.TargetMS != 1001 {
		t.Fatalf("GetMessage = %+v, %v", m, err)
	}
	p, err := s.Progress(ctx, "r1")
	if err != nil || len(p.Stages) != 6 || p.Stages[4].Stage != store.StageFiles || p.Stages[5].Stage != store.StageLoad || p.Files != 0 {
		t.Fatalf("Progress = %+v, %v", p.Stages, err)
	}
}

// A plan with a files lane: accepted, and claimed after team, before load.
func TestFilesStageInPlan(t *testing.T) {
	s := open(t)
	id := newRun(t, s, "acme")
	p := testPlan([]string{"C1"}, 1)
	p.Lanes = append(p.Lanes, store.Lane{ID: "files:C1", Stage: store.StageFiles, Key: "C1"})
	p.Batches = append(p.Batches, store.Batch{LaneID: "files:C1", Seq: 0, Key: "2024-03-01"})
	p.Channels[0].FilesDriveID, p.Channels[0].FilesFolderID = "drive-1", "folder-1"
	applyPlan(t, s, id, p)
	if c, err := s.Channel(t.Context(), id, "C1"); err != nil || c.FilesDriveID != "drive-1" || c.FilesFolderID != "folder-1" {
		t.Fatalf("Channel = %+v, %v", c, err)
	}
	// Make everything claimable at once.
	exec(t, s, `DELETE FROM batch_deps`)
	var got []string
	for {
		l, err := s.Claim(t.Context(), store.ClaimFilter{Stages: allStages, RunID: id}, "h", ttl)
		if err != nil {
			t.Fatal(err)
		}
		if l == nil {
			break
		}
		got = append(got, l.Stage)
	}
	want := []string{"extract", "transform", "team", "files", "load"}
	if len(got) != len(want) {
		t.Fatalf("claim order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("claim order = %v, want %v", got, want)
		}
	}
	// The stages filter selects the files stage alone.
	noClaim(t, s, id, store.StageFiles)
}

func file(ch, id string, size int64, seq int) store.FileMapping {
	return store.FileMapping{ChannelID: ch, SlackID: id, Name: id + ".pdf", Mimetype: "application/pdf", Size: size, BatchSeq: seq}
}

func TestFilesLifecycle(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2"}, 1)
	l := mustClaim(t, s, id, "x", store.StageExtract)
	do := func(fn func(tx store.Tx) error) error { return s.Fenced(ctx, l, fn) }
	get := func(ch, fid string) store.FileMapping {
		t.Helper()
		f, err := s.GetFile(ctx, id, ch, fid)
		if err != nil {
			t.Fatalf("GetFile(%s/%s): %v", ch, fid, err)
		}
		return f
	}
	snapshot := func() string {
		return scalar[string](t, s, `SELECT md5(string_agg(f::text, '|' ORDER BY channel_id, slack_id)) FROM map_files f`)
	}

	// --- PutFiles ---
	skipped := file("C1", "F0EXT", 0, 0)
	skipped.State, skipped.ErrorCode, skipped.Error = store.FileSkipped, "external", "hosted on Google Drive"
	in := []store.FileMapping{
		file("C1", "F03", 300, 0), file("C1", "F01", 100, 0), file("C1", "F02", 200, 0), file("C1", "F10", 1000, 1),
		file("C2", "F01", 100, 0), // the same Slack file shared in another channel
		skipped,
	}
	// Staging and upload fields in the input are ignored.
	in[0].StagingKey, in[0].ItemID, in[0].SHA256, in[0].ErrorCode = "bogus", "bogus", "bogus", "bogus"
	if err := do(func(tx store.Tx) error { return tx.PutFiles(ctx, in) }); err != nil {
		t.Fatalf("PutFiles: %v", err)
	}
	if err := do(func(tx store.Tx) error { return tx.PutFiles(ctx, nil) }); err != nil {
		t.Fatalf("PutFiles(nothing): %v", err)
	}
	f := get("C1", "F03")
	if f != (store.FileMapping{ChannelID: "C1", SlackID: "F03", Name: "F03.pdf", Mimetype: "application/pdf", Size: 300, State: store.FilePending}) {
		t.Fatalf("new file = %+v", f)
	}
	if f := get("C1", "F0EXT"); f.State != store.FileSkipped || f.ErrorCode != "external" || f.Error != "hosted on Google Drive" {
		t.Fatalf("file inserted as skipped = %+v", f)
	}
	_, err := s.GetFile(ctx, id, "C1", "NOPE")
	wantErr(t, err, store.ErrNotFound)
	_, err = s.GetFile(ctx, id, "C2", "F02")
	wantErr(t, err, store.ErrNotFound)

	// FilesInBatch: one channel's batch, ordered by Slack file ID.
	batch, err := s.FilesInBatch(ctx, id, "C1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range batch {
		order = append(order, f.SlackID)
	}
	if want := []string{"F01", "F02", "F03", "F0EXT"}; len(order) != 4 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] || order[3] != want[3] {
		t.Fatalf("FilesInBatch order = %v, want %v", order, want)
	}
	if b, _ := s.FilesInBatch(ctx, id, "C1", 1); len(b) != 1 || b[0].SlackID != "F10" || b[0].BatchSeq != 1 {
		t.Fatalf("batch 1 = %+v", b)
	}
	if b, _ := s.FilesInBatch(ctx, id, "C2", 0); len(b) != 1 || b[0].ChannelID != "C2" {
		t.Fatalf("C2 batch 0 = %+v", b)
	}
	if b, _ := s.FilesInBatch(ctx, id, "C1", 9); len(b) != 0 {
		t.Fatalf("empty batch = %+v", b)
	}

	// Refusals at insert.
	for _, bad := range []store.FileMapping{
		{ChannelID: "C1", SlackID: "FX", State: store.FileStaged},
		{ChannelID: "C1", SlackID: "FX", State: store.FileUploaded},
		{ChannelID: "C1", SlackID: "FX", State: "weird"},
		{ChannelID: "C1", SlackID: ""},
		{ChannelID: "", SlackID: "FX"},
		{ChannelID: "C1", SlackID: "FX", Size: -1},
		{ChannelID: "NOPE", SlackID: "FX"},
	} {
		if err := do(func(tx store.Tx) error { return tx.PutFiles(ctx, []store.FileMapping{bad}) }); err == nil {
			t.Errorf("PutFiles(%+v) succeeded", bad)
		}
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM map_files`); n != 6 {
		t.Fatalf("%d file rows, want 6", n)
	}

	// --- staged ---
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFileStaged(ctx, "C1", "NOPE", "k", "h", 1) }), store.ErrNotFound)
	if err := do(func(tx store.Tx) error { return tx.MarkFileStaged(ctx, "C1", "F01", "", "h", 1) }); err == nil {
		t.Error("MarkFileStaged without a key succeeded")
	}
	if err := do(func(tx store.Tx) error { return tx.MarkFileStaged(ctx, "C1", "F01", "stage/F01", "sha-1", 101) }); err != nil {
		t.Fatal(err)
	}
	f = get("C1", "F01")
	if f.State != store.FileStaged || f.StagingKey != "stage/F01" || f.SHA256 != "sha-1" || f.StagedBytes != 101 || f.Size != 100 {
		t.Fatalf("staged file = %+v", f)
	}
	// The same key and digest again: nothing changes at all.
	before := snapshot()
	if err := do(func(tx store.Tx) error { return tx.MarkFileStaged(ctx, "C1", "F01", "stage/F01", "sha-1", 999) }); err != nil {
		t.Fatalf("MarkFileStaged again: %v", err)
	}
	if snapshot() != before {
		t.Fatal("a repeated MarkFileStaged changed the row")
	}
	// The other channel's row for the same Slack file is separate.
	if f := get("C2", "F01"); f.State != store.FilePending {
		t.Fatalf("C2/F01 = %+v", f)
	}

	// --- settled ---
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFileSettled(ctx, "C1", "NOPE", store.FileFailed, "c", "r") }), store.ErrNotFound)
	for _, bad := range []store.FileState{store.FilePending, store.FileStaged, store.FileUploaded, "weird", ""} {
		if err := do(func(tx store.Tx) error { return tx.MarkFileSettled(ctx, "C1", "F02", bad, "c", "r") }); err == nil {
			t.Errorf("MarkFileSettled(%q) succeeded", bad)
		}
	}
	if f := get("C1", "F02"); f.State != store.FilePending {
		t.Fatalf("refused MarkFileSettled changed the row: %+v", f)
	}
	if err := do(func(tx store.Tx) error {
		return tx.MarkFileSettled(ctx, "C1", "F02", store.FileFailed, "download_403", "token lacks files:read")
	}); err != nil {
		t.Fatal(err)
	}
	if f := get("C1", "F02"); f.State != store.FileFailed || f.ErrorCode != "download_403" || f.Error != "token lacks files:read" {
		t.Fatalf("failed file = %+v", f)
	}
	// failed -> staged (a retry), clearing the error.
	if err := do(func(tx store.Tx) error { return tx.MarkFileStaged(ctx, "C1", "F02", "stage/F02", "sha-2", 200) }); err != nil {
		t.Fatal(err)
	}
	if f := get("C1", "F02"); f.State != store.FileStaged || f.ErrorCode != "" || f.Error != "" || f.StagingKey != "stage/F02" {
		t.Fatalf("re-staged file = %+v", f)
	}
	// staged -> failed (the upload failed) keeps the staging facts.
	if err := do(func(tx store.Tx) error {
		return tx.MarkFileSettled(ctx, "C1", "F02", store.FileFailed, "upload_507", "quota")
	}); err != nil {
		t.Fatal(err)
	}
	if f := get("C1", "F02"); f.State != store.FileFailed || f.StagingKey != "stage/F02" || f.SHA256 != "sha-2" || f.StagedBytes != 200 {
		t.Fatalf("failed after staging = %+v", f)
	}
	if err := do(func(tx store.Tx) error {
		return tx.MarkFileSettled(ctx, "C1", "F03", store.FileSkipped, "too_large", "over 250 GB")
	}); err != nil {
		t.Fatal(err)
	}
	if f := get("C1", "F03"); f.State != store.FileSkipped || f.ErrorCode != "too_large" {
		t.Fatalf("skipped file = %+v", f)
	}

	// --- uploaded ---
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFileUploaded(ctx, "C1", "NOPE", "d", "i", "u", "e") }), store.ErrNotFound)
	if err := do(func(tx store.Tx) error { return tx.MarkFileUploaded(ctx, "C1", "F01", "d", "", "u", "e") }); err == nil {
		t.Error("MarkFileUploaded without an item ID succeeded")
	}
	if err := do(func(tx store.Tx) error {
		return tx.MarkFileUploaded(ctx, "C1", "F01", "drive-1", "item-1", "https://sp/F01.pdf", `"etag-1"`)
	}); err != nil {
		t.Fatal(err)
	}
	f = get("C1", "F01")
	if f.State != store.FileUploaded || f.DriveID != "drive-1" || f.ItemID != "item-1" || f.WebDavURL != "https://sp/F01.pdf" ||
		f.ETag != `"etag-1"` || f.StagingKey != "stage/F01" || f.SHA256 != "sha-1" {
		t.Fatalf("uploaded file = %+v", f)
	}
	before = snapshot()
	// The same item again is a no-op and keeps the first URL and eTag.
	if err := do(func(tx store.Tx) error {
		return tx.MarkFileUploaded(ctx, "C1", "F01", "drive-1", "item-1", "https://other", "other")
	}); err != nil {
		t.Fatalf("MarkFileUploaded again: %v", err)
	}
	// A different item, or the same item ID in another drive, is refused.
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFileUploaded(ctx, "C1", "F01", "drive-1", "item-2", "u", "e") }), store.ErrTargetConflict)
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFileUploaded(ctx, "C1", "F01", "drive-2", "item-1", "u", "e") }), store.ErrTargetConflict)
	// An uploaded file is never moved backwards or overwritten.
	if err := do(func(tx store.Tx) error { return tx.MarkFileStaged(ctx, "C1", "F01", "stage/other", "sha-other", 5) }); err != nil {
		t.Fatalf("MarkFileStaged on an uploaded file: %v, want a no-op", err)
	}
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFileSettled(ctx, "C1", "F01", store.FileFailed, "c", "r") }), store.ErrTargetConflict)
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFileSettled(ctx, "C1", "F01", store.FileSkipped, "c", "r") }), store.ErrTargetConflict)
	if snapshot() != before {
		t.Fatal("an uploaded file was changed")
	}
	// failed -> uploaded directly (found already in SharePoint on retry).
	if err := do(func(tx store.Tx) error {
		return tx.MarkFileUploaded(ctx, "C1", "F02", "drive-1", "item-9", "https://sp/F02.pdf", "e9")
	}); err != nil {
		t.Fatal(err)
	}
	if f := get("C1", "F02"); f.State != store.FileUploaded || f.ErrorCode != "" || f.Error != "" || f.ItemID != "item-9" {
		t.Fatalf("failed then uploaded = %+v", f)
	}

	// --- PutFiles idempotency: nothing above is disturbed by a re-run ---
	before = snapshot()
	again := []store.FileMapping{file("C1", "F01", 1, 5), file("C1", "F02", 2, 5), file("C1", "F03", 3, 5), file("C1", "F99", 99, 1)}
	again[0].Name, again[2].State = "renamed", store.FileFailed
	if err := do(func(tx store.Tx) error { return tx.PutFiles(ctx, again) }); err != nil {
		t.Fatalf("PutFiles re-run: %v", err)
	}
	after := scalar[string](t, s, `SELECT md5(string_agg(f::text, '|' ORDER BY channel_id, slack_id)) FROM map_files f WHERE slack_id <> 'F99'`)
	if after != before {
		t.Fatal("PutFiles changed existing rows")
	}
	if f := get("C1", "F99"); f.State != store.FilePending || f.Size != 99 {
		t.Fatalf("new row from a re-run = %+v", f)
	}
	// Two rows with the same key in one call: the first wins.
	if err := do(func(tx store.Tx) error {
		return tx.PutFiles(ctx, []store.FileMapping{file("C2", "FDUP", 1, 0), file("C2", "FDUP", 2, 0)})
	}); err != nil {
		t.Fatalf("PutFiles with a duplicate: %v", err)
	}
	if f := get("C2", "FDUP"); f.Size != 1 {
		t.Fatalf("duplicate in one call = %+v", f)
	}
}

func TestSetChannelFolder(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2"}, 1)
	l := mustClaim(t, s, id, "x", store.StageExtract)
	do := func(fn func(tx store.Tx) error) error { return s.Fenced(ctx, l, fn) }

	wantErr(t, do(func(tx store.Tx) error { return tx.SetChannelFolder(ctx, "NOPE", "d", "f") }), store.ErrNotFound)
	if err := do(func(tx store.Tx) error { return tx.SetChannelFolder(ctx, "C1", "", "f") }); err == nil {
		t.Error("SetChannelFolder without a drive succeeded")
	}
	if err := do(func(tx store.Tx) error { return tx.SetChannelFolder(ctx, "C1", "d", "") }); err == nil {
		t.Error("SetChannelFolder without a folder succeeded")
	}
	if err := do(func(tx store.Tx) error { return tx.SetChannelFolder(ctx, "C1", "drive-1", "folder-1") }); err != nil {
		t.Fatal(err)
	}
	if err := do(func(tx store.Tx) error { return tx.SetChannelFolder(ctx, "C1", "drive-1", "folder-1") }); err != nil {
		t.Fatalf("same folder again: %v", err)
	}
	wantErr(t, do(func(tx store.Tx) error { return tx.SetChannelFolder(ctx, "C1", "drive-1", "folder-2") }), store.ErrTargetConflict)
	wantErr(t, do(func(tx store.Tx) error { return tx.SetChannelFolder(ctx, "C1", "drive-2", "folder-1") }), store.ErrTargetConflict)
	// SetChannel does not disturb the folder.
	if err := do(func(tx store.Tx) error { return tx.SetChannel(ctx, "C1", "19:c1", store.ChannelCreated, nil) }); err != nil {
		t.Fatal(err)
	}
	chans, err := s.Channels(ctx, id)
	if err != nil || len(chans) != 2 {
		t.Fatal(err)
	}
	if c := chans[0]; c.SlackID != "C1" || c.FilesDriveID != "drive-1" || c.FilesFolderID != "folder-1" || c.TeamsID != "19:c1" {
		t.Fatalf("C1 = %+v", c)
	}
	if c := chans[1]; c.FilesDriveID != "" || c.FilesFolderID != "" {
		t.Fatalf("C2 = %+v", c)
	}
}

// A holder that lost its lane cannot record anything about files.
func TestFilesFencing(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 2)
	a := mustClaim(t, s, id, "A", store.StageExtract)
	if err := s.Fenced(ctx, a, func(tx store.Tx) error {
		return tx.PutFiles(ctx, []store.FileMapping{file("C1", "F01", 100, 0), file("C1", "F02", 200, 0)})
	}); err != nil {
		t.Fatal(err)
	}
	// A stalls; B takes the lane over.
	exec(t, s, `UPDATE lanes SET lease_expires = now() - interval '1 second' WHERE run_id = $1 AND id = $2`, id, a.LaneID)
	b := mustClaim(t, s, id, "B", store.StageExtract)
	if b.LaneID != a.LaneID || b.Fence != a.Fence+1 {
		t.Fatalf("B's lease = %+v", b)
	}
	before := scalar[string](t, s, `SELECT md5(string_agg(f::text, '|' ORDER BY slack_id)) || (SELECT md5(string_agg(c::text, '|')) FROM map_channels c) FROM map_files f`)

	ops := map[string]func(tx store.Tx) error{
		"PutFiles":         func(tx store.Tx) error { return tx.PutFiles(ctx, []store.FileMapping{file("C1", "F03", 300, 0)}) },
		"MarkFileStaged":   func(tx store.Tx) error { return tx.MarkFileStaged(ctx, "C1", "F01", "k", "h", 1) },
		"MarkFileUploaded": func(tx store.Tx) error { return tx.MarkFileUploaded(ctx, "C1", "F01", "d", "i", "u", "e") },
		"MarkFileSettled":  func(tx store.Tx) error { return tx.MarkFileSettled(ctx, "C1", "F02", store.FileFailed, "c", "r") },
		"SetChannelFolder": func(tx store.Tx) error { return tx.SetChannelFolder(ctx, "C1", "d", "f") },
	}
	for name, op := range ops {
		called := false
		err := s.Fenced(ctx, a, func(tx store.Tx) error { called = true; return op(tx) })
		if !errors.Is(err, store.ErrFenced) || called {
			t.Errorf("%s under a lost lease: err = %v, fn called = %v", name, err, called)
		}
	}
	after := scalar[string](t, s, `SELECT md5(string_agg(f::text, '|' ORDER BY slack_id)) || (SELECT md5(string_agg(c::text, '|')) FROM map_channels c) FROM map_files f`)
	if before != after {
		t.Fatal("a fenced-out holder changed file or channel rows")
	}
	// The current holder can.
	for name, op := range ops {
		if err := s.Fenced(ctx, b, op); err != nil {
			t.Errorf("%s under the current lease: %v", name, err)
		}
	}
	// And a transaction that fails takes its file writes with it.
	boom := errors.New("boom")
	err := s.Fenced(ctx, b, func(tx store.Tx) error {
		if err := tx.MarkFileStaged(ctx, "C1", "F03", "k3", "h3", 3); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("Fenced = %v", err)
	}
	if f, _ := s.GetFile(ctx, id, "C1", "F03"); f.State != store.FilePending {
		t.Fatalf("rolled-back staging is visible: %+v", f)
	}
}

func TestProgressFiles(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2"}, 1)
	p, err := s.Progress(ctx, id)
	if err != nil || p.Files != 0 || p.FileBytes != 0 || p.FileBytesDone != 0 {
		t.Fatalf("Progress without files = %+v, %v", p, err)
	}
	l := mustClaim(t, s, id, "x", store.StageExtract)
	err = s.Fenced(ctx, l, func(tx store.Tx) error {
		if err := tx.PutFiles(ctx, []store.FileMapping{
			file("C1", "F1", 1_000, 0),         // uploaded
			file("C1", "F2", 20_000, 0),        // uploaded
			file("C1", "F3", 300_000, 0),       // staged
			file("C1", "F4", 4_000_000, 0),     // pending
			file("C2", "F5", 50_000_000, 0),    // skipped
			file("C2", "F6", 600_000_000, 0),   // failed
			file("C2", "F7", 7_000_000_000, 0), // failed, larger than int32
			file("C2", "F1", 1_000, 0),         // uploaded (same Slack file, other channel)
		}); err != nil {
			return err
		}
		for _, k := range [][2]string{{"C1", "F1"}, {"C1", "F2"}, {"C1", "F3"}, {"C2", "F1"}} {
			if err := tx.MarkFileStaged(ctx, k[0], k[1], "stage/"+k[0]+k[1], "sha", 1); err != nil {
				return err
			}
		}
		for _, k := range [][2]string{{"C1", "F1"}, {"C1", "F2"}, {"C2", "F1"}} {
			if err := tx.MarkFileUploaded(ctx, k[0], k[1], "d", "item-"+k[0]+k[1], "u", "e"); err != nil {
				return err
			}
		}
		if err := tx.MarkFileSettled(ctx, "C2", "F5", store.FileSkipped, "external", "x"); err != nil {
			return err
		}
		if err := tx.MarkFileSettled(ctx, "C2", "F6", store.FileFailed, "c", "x"); err != nil {
			return err
		}
		return tx.MarkFileSettled(ctx, "C2", "F7", store.FileFailed, "c", "x")
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second run's files do not leak into the first run's totals.
	other := plannedRun(t, s, "other", []string{"C1"}, 1)
	ol := mustClaim(t, s, other, "y", store.StageExtract)
	if err := s.Fenced(ctx, ol, func(tx store.Tx) error { return tx.PutFiles(ctx, []store.FileMapping{file("C1", "F1", 9, 0)}) }); err != nil {
		t.Fatal(err)
	}

	p, err = s.Progress(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	const total = 1_000 + 20_000 + 300_000 + 4_000_000 + 50_000_000 + 600_000_000 + 7_000_000_000 + 1_000
	if p.Files != 8 || p.FilesUploaded != 3 || p.FilesSkipped != 1 || p.FilesFailed != 2 || p.FileBytes != total || p.FileBytesDone != 22_000 {
		t.Fatalf("file totals: files=%d uploaded=%d skipped=%d failed=%d bytes=%d done=%d",
			p.Files, p.FilesUploaded, p.FilesSkipped, p.FilesFailed, p.FileBytes, p.FileBytesDone)
	}
	if op, _ := s.Progress(ctx, other); op.Files != 1 || op.FileBytes != 9 {
		t.Fatalf("other run: %+v", op)
	}
}

// Operators retry while controller replicas reconcile and workers keep failing
// batches. The run must never be left failed while it has open lanes with
// work to do: a failed run is not claimable, so that state would strand it.
func TestRetryFailedVersusReconcile(t *testing.T) {
	s := open(t)
	ctx := context.WithoutCancel(t.Context())
	// Extract lanes only, so that when they have all failed nothing else
	// is runnable and Reconcile does fail the run.
	id := newRun(t, s, "acme")
	p := store.Plan{}
	for _, c := range []string{"C1", "C2", "C3"} {
		p.Lanes = append(p.Lanes, store.Lane{ID: "extract:" + c, Stage: store.StageExtract, Key: c})
		p.Batches = append(p.Batches, store.Batch{LaneID: "extract:" + c, Seq: 0}, store.Batch{LaneID: "extract:" + c, Seq: 1})
	}
	applyPlan(t, s, id, p)

	// One statement, one snapshot: a failed run with an open lane that has
	// a pending batch (extract lanes have no dependencies).
	const strandedSQL = `
		SELECT count(*) FROM runs r
		WHERE r.id = $1 AND r.status = 'failed'
		  AND EXISTS (
		      SELECT 1 FROM lanes l
		      JOIN batches b ON b.run_id = l.run_id AND b.lane_id = l.id AND b.status = 'pending'
		      WHERE l.run_id = r.id AND l.status = 'open' AND l.stage = 'extract')`

	var (
		stop                           atomic.Bool
		wg                             sync.WaitGroup
		stranded, retries, reconciles  atomic.Int64
		markedFailed, lanesFailedCount atomic.Int64
	)
	report := func(format string, args ...any) {
		t.Errorf(format, args...)
		stop.Store(true)
	}
	// Workers: every extract batch fails permanently.
	for w := range 4 {
		wg.Go(func() {
			holder := "worker-" + string(rune('a'+w))
			for !stop.Load() {
				l, err := s.Claim(ctx, store.ClaimFilter{Stages: []string{store.StageExtract}, RunID: id}, holder, ttl)
				if err != nil {
					report("Claim: %v", err)
					return
				}
				if l == nil {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				b, err := s.NextBatch(ctx, l)
				if err == nil && b != nil {
					err = s.Fenced(ctx, l, func(tx store.Tx) error { return tx.FailBatch(ctx, b.Seq, "always fails") })
					if err == nil {
						lanesFailedCount.Add(1)
					}
				}
				if err != nil && !errors.Is(err, store.ErrFenced) {
					report("worker: %v", err)
					return
				}
				if err := s.Release(ctx, l, 0, ""); err != nil {
					report("Release: %v", err)
					return
				}
			}
		})
	}
	// controller replicas.
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				changed, err := s.Reconcile(ctx)
				if err != nil {
					report("Reconcile: %v", err)
					return
				}
				reconciles.Add(1)
				for _, r := range changed {
					if r.Status == store.RunFailed {
						markedFailed.Add(1)
					}
				}
			}
		})
	}
	// Operators.
	for range 2 {
		wg.Go(func() {
			for !stop.Load() {
				if _, err := s.RetryFailed(ctx, id); err != nil {
					report("RetryFailed: %v", err)
					return
				}
				retries.Add(1)
				time.Sleep(time.Millisecond)
			}
		})
	}
	// The invariant, checked continuously.
	wg.Go(func() {
		for !stop.Load() {
			var n int
			if err := s.Pool().QueryRow(ctx, strandedSQL, id).Scan(&n); err != nil {
				report("check: %v", err)
				return
			}
			if n != 0 {
				stranded.Add(1)
			}
		}
	})
	time.Sleep(2500 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	t.Logf("%d retries, %d reconciles (%d marked the run failed), %d batches failed, %d stranded observations",
		retries.Load(), reconciles.Load(), markedFailed.Load(), lanesFailedCount.Load(), stranded.Load())
	if stranded.Load() != 0 {
		t.Fatalf("the run was observed failed with open, runnable lanes %d times", stranded.Load())
	}
	if markedFailed.Load() == 0 || lanesFailedCount.Load() == 0 {
		t.Fatal("the test did not exercise the failed transition")
	}

	// At rest: one more retry leaves the run running and claimable, and a
	// reconcile does not fail it again.
	if _, err := s.RetryFailed(ctx, id); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.Reconcile(ctx); err != nil || len(changed) != 0 {
		t.Fatalf("Reconcile after the final retry = %+v, %v", changed, err)
	}
	if st := runStatus(t, s, id); st != store.RunRunning {
		t.Fatalf("status = %s, want running", st)
	}
	if n := scalar[int](t, s, strandedSQL, id); n != 0 {
		t.Fatal("run stranded at rest")
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes l JOIN batches b ON b.run_id = l.run_id AND b.lane_id = l.id WHERE l.run_id = $1 AND l.status = 'open' AND b.status = 'failed'`, id); n != 0 {
		t.Fatalf("%d failed batches sit in open lanes", n)
	}
	mustClaim(t, s, id, "h", store.StageExtract)
}

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SetChannelFolder records the channel's SharePoint files folder. The
// channel must be in the plan (ErrNotFound). Once recorded the folder is
// never replaced: the same drive and folder again is a no-op, a different
// one returns ErrTargetConflict.
func (t *fencedTx) SetChannelFolder(ctx context.Context, slackID, driveID, folderID string) error {
	if err := t.check(); err != nil {
		return err
	}
	if driveID == "" || folderID == "" {
		return errors.New("store: SetChannelFolder needs a drive ID and a folder ID")
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_channels SET files_drive_id = $3, files_folder_id = $4
		WHERE run_id = $1 AND slack_id = $2 AND files_drive_id IS NULL`,
		t.lease.RunID, slackID, driveID, folderID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var curDrive, curFolder string
	err = t.tx.QueryRow(ctx, `
		SELECT COALESCE(files_drive_id, ''), COALESCE(files_folder_id, '') FROM map_channels
		WHERE run_id = $1 AND slack_id = $2`, t.lease.RunID, slackID).Scan(&curDrive, &curFolder)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: channel %s", ErrNotFound, slackID)
	}
	if err != nil {
		return err
	}
	if curDrive != driveID || curFolder != folderID {
		return fmt.Errorf("%w: channel %s already has files folder %s/%s, refusing to record %s/%s",
			ErrTargetConflict, slackID, curDrive, curFolder, driveID, folderID)
	}
	return nil
}

// PutFiles inserts file rows. A row whose key (channel, Slack file ID)
// already exists is left completely untouched, whatever the new values are
// (ON CONFLICT on the primary key DO NOTHING), so re-running the batch that
// discovers files cannot disturb one that is already staged or uploaded.
//
// A new row may start as pending (the default when State is empty), or
// directly as skipped or failed with its ErrorCode and Error, for files
// known at discovery not to be copyable. Staged and uploaded are refused:
// those states carry facts that only MarkFileStaged and MarkFileUploaded
// record. Of the input, ChannelID, SlackID, Name, Mimetype, Size, State,
// ErrorCode, Error and BatchSeq are stored; the staging and driveItem
// fields are ignored.
func (t *fencedTx) PutFiles(ctx context.Context, files []FileMapping) error {
	if err := t.check(); err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	var (
		channels = make([]string, len(files))
		ids      = make([]string, len(files))
		names    = make([]string, len(files))
		mimes    = make([]string, len(files))
		sizes    = make([]int64, len(files))
		states   = make([]string, len(files))
		codes    = make([]string, len(files))
		reasons  = make([]string, len(files))
		seqs     = make([]int32, len(files))
	)
	for i, f := range files {
		if f.ChannelID == "" || f.SlackID == "" {
			return fmt.Errorf("store: file %d has no channel ID or Slack file ID", i)
		}
		state := f.State
		if state == "" {
			state = FilePending
		}
		switch state {
		case FilePending, FileSkipped, FileFailed:
		default:
			return fmt.Errorf("store: file %s/%s cannot be inserted as %q; use MarkFileStaged or MarkFileUploaded", f.ChannelID, f.SlackID, f.State)
		}
		if f.Size < 0 {
			return fmt.Errorf("store: file %s/%s has a negative size", f.ChannelID, f.SlackID)
		}
		seq, err := int32Of("file batch seq", f.BatchSeq)
		if err != nil {
			return err
		}
		channels[i], ids[i], names[i], mimes[i], sizes[i] = f.ChannelID, f.SlackID, f.Name, f.Mimetype, f.Size
		states[i], seqs[i] = string(state), seq
		if state != FilePending {
			codes[i], reasons[i] = f.ErrorCode, f.Error
		}
	}
	_, err := t.tx.Exec(ctx, `
		INSERT INTO map_files (run_id, channel_id, slack_id, name, mimetype, size, state, error_code, error, batch_seq)
		SELECT $1, u.channel_id, u.slack_id, u.name, u.mimetype, u.size, u.state, u.error_code, u.error, u.batch_seq
		FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::bigint[], $7::text[], $8::text[], $9::text[], $10::int[])
		     AS u(channel_id, slack_id, name, mimetype, size, state, error_code, error, batch_seq)
		ON CONFLICT (run_id, channel_id, slack_id) DO NOTHING`,
		t.lease.RunID, channels, ids, names, mimes, sizes, states, codes, reasons, seqs)
	if code, _ := pgCode(err); code == sqlForeignKeyViolation {
		return fmt.Errorf("store: PutFiles: a file refers to a channel that is not in the plan: %w", err)
	}
	return err
}

// fileRow reads the fields the Mark* methods decide on, or ErrNotFound.
func (t *fencedTx) fileRow(ctx context.Context, channelID, slackID string) (f FileMapping, err error) {
	var state string
	err = t.tx.QueryRow(ctx, `
		SELECT state, staging_key, sha256, drive_id, item_id FROM map_files
		WHERE run_id = $1 AND channel_id = $2 AND slack_id = $3`,
		t.lease.RunID, channelID, slackID).Scan(&state, &f.StagingKey, &f.SHA256, &f.DriveID, &f.ItemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, fmt.Errorf("%w: file %s/%s", ErrNotFound, channelID, slackID)
	}
	f.State = FileState(state)
	return f, err
}

// MarkFileStaged records that a file's content is in staging: the staging
// key, the hex SHA-256 of the content and its size in bytes. The row must
// exist (ErrNotFound).
//
// A pending, failed or skipped file becomes staged and its error fields are
// cleared. A file that is already staged with the same key and digest is
// left as it is; one staged with a different key or digest is updated to
// the new content (nothing outside Taut refers to staged content yet). An
// uploaded file is never moved backwards: the call is a no-op and the
// recorded staging facts are kept.
//
// These rules are applied by one conditional UPDATE under the lane's lock,
// so there is no window between reading the state and changing it.
func (t *fencedTx) MarkFileStaged(ctx context.Context, channelID, slackID, stagingKey, sha256 string, bytes int64) error {
	if err := t.check(); err != nil {
		return err
	}
	if stagingKey == "" || sha256 == "" {
		return errors.New("store: MarkFileStaged needs the staging key and the content's SHA-256")
	}
	if bytes < 0 {
		return errors.New("store: MarkFileStaged: negative size")
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_files
		SET state = 'staged', staging_key = $4, sha256 = $5, staged_bytes = $6,
		    error_code = '', error = '', updated_at = now()
		WHERE run_id = $1 AND channel_id = $2 AND slack_id = $3
		  AND state <> 'uploaded'
		  AND NOT (state = 'staged' AND staging_key = $4 AND sha256 = $5)`,
		t.lease.RunID, channelID, slackID, stagingKey, sha256, bytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing changed: missing, already staged identically, or uploaded.
	_, err = t.fileRow(ctx, channelID, slackID)
	return err
}

// MarkFileUploaded records the driveItem a file became, from any state that
// is not uploaded, and clears its error fields. The row must exist
// (ErrNotFound). An uploaded file is never changed: the same item (drive ID
// and item ID) again is a no-op that keeps the recorded URL and eTag, a
// different item returns ErrTargetConflict.
func (t *fencedTx) MarkFileUploaded(ctx context.Context, channelID, slackID, driveID, itemID, webDavURL, etag string) error {
	if err := t.check(); err != nil {
		return err
	}
	if driveID == "" || itemID == "" {
		return errors.New("store: MarkFileUploaded needs the drive ID and the item ID")
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_files
		SET state = 'uploaded', drive_id = $4, item_id = $5, webdav_url = $6, etag = $7,
		    error_code = '', error = '', updated_at = now()
		WHERE run_id = $1 AND channel_id = $2 AND slack_id = $3 AND state <> 'uploaded'`,
		t.lease.RunID, channelID, slackID, driveID, itemID, webDavURL, etag)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	cur, err := t.fileRow(ctx, channelID, slackID)
	if err != nil {
		return err
	}
	if cur.DriveID != driveID || cur.ItemID != itemID {
		return fmt.Errorf("%w: file %s/%s is uploaded as item %s/%s, refusing to record %s/%s",
			ErrTargetConflict, channelID, slackID, cur.DriveID, cur.ItemID, driveID, itemID)
	}
	return nil
}

// MarkFileSettled records a file that will not be uploaded: state must be
// FileSkipped (not copied by design) or FileFailed (could not be downloaded
// or uploaded), with a code and a reason. The row must exist (ErrNotFound).
// Staging facts already recorded are kept. An uploaded file is never
// overwritten: that returns ErrTargetConflict.
func (t *fencedTx) MarkFileSettled(ctx context.Context, channelID, slackID string, state FileState, code, reason string) error {
	if err := t.check(); err != nil {
		return err
	}
	if state != FileSkipped && state != FileFailed {
		return fmt.Errorf("store: MarkFileSettled state must be %q or %q, not %q", FileSkipped, FileFailed, state)
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE map_files
		SET state = $4, error_code = $5, error = $6, updated_at = now()
		WHERE run_id = $1 AND channel_id = $2 AND slack_id = $3 AND state <> 'uploaded'`,
		t.lease.RunID, channelID, slackID, string(state), code, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	cur, err := t.fileRow(ctx, channelID, slackID)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: file %s/%s is already uploaded as item %s/%s and cannot be marked %s",
		ErrTargetConflict, channelID, slackID, cur.DriveID, cur.ItemID, state)
}

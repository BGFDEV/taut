package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Ledger is what a run recorded as being in Teams: the starting point of
// the pass that follows it.
type Ledger struct {
	RunID  string    `json:"runId"`
	Pass   int       `json:"pass"`
	Status RunStatus `json:"status"`
	// Planned is false for a run that never got as far as a plan; it has
	// nothing in Teams.
	Planned     bool   `json:"planned"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	// Config is the run's config file, which says which tenant it wrote to.
	Config   []byte          `json:"-"`
	Teams    []LedgerTeam    `json:"teams"`
	Channels []LedgerChannel `json:"channels"`
	// Users are the Slack user IDs the run's plan knew, in byte order.
	Users []string `json:"users"`
	// Posted and Files total the channels' counts.
	Posted int `json:"posted"`
	Files  int `json:"files"`
}

// LedgerTeam is one team a run wrote to.
type LedgerTeam struct {
	Name    string `json:"name"`
	TeamsID string `json:"teamsId,omitempty"`
}

// LedgerChannel is one conversation of a run's ledger.
type LedgerChannel struct {
	SlackID   string `json:"slackId"`
	SlackName string `json:"slackName,omitempty"`
	Kind      string `json:"kind"`
	// Team is the name of the channel's team; empty for a chat.
	Team       string       `json:"team,omitempty"`
	TargetName string       `json:"targetName,omitempty"`
	State      ChannelState `json:"state"`
	// InTeams is true when the conversation has a Teams ID.
	InTeams bool `json:"inTeams"`
	// Posted counts the Slack messages with a post in Teams (a message
	// that was split into parts counts once); Files the uploaded files.
	Posted int `json:"posted"`
	Files  int `json:"files"`
}

// Passes returns the chain of runs that ends at runID, pass 1 first.
func (s *PG) Passes(ctx context.Context, runID string) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT r.* FROM runs r WHERE r.id = $1
			UNION ALL
			SELECT b.* FROM runs b JOIN chain c ON b.id = c.base_run_id
		)
		SELECT `+runCols+` FROM chain ORDER BY pass, created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// Follower returns the pass that follows runID, if there is one. CreateRun
// lets at most one run that is not cancelled name a given base.
func (s *PG) Follower(ctx context.Context, runID string) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `
		SELECT `+runCols+` FROM runs
		WHERE base_run_id = $1 AND status <> 'cancelled'
		ORDER BY created_at DESC, id DESC LIMIT 1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// Ledger summarizes what a run recorded as being in Teams. The counts and
// the lists describe one instant.
func (s *PG) Ledger(ctx context.Context, runID string) (Ledger, error) {
	tx, err := s.pool.BeginTx(ctx, repeatableRead)
	if err != nil {
		return Ledger{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	l := Ledger{RunID: runID, Teams: []LedgerTeam{}, Channels: []LedgerChannel{}, Users: []string{}}
	var status string
	err = tx.QueryRow(ctx, `
		SELECT pass, status, planned_at IS NOT NULL, workspace_id, config FROM runs WHERE id = $1`, runID).
		Scan(&l.Pass, &status, &l.Planned, &l.WorkspaceID, &l.Config)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ledger{}, ErrNotFound
	}
	if err != nil {
		return Ledger{}, err
	}
	l.Status = RunStatus(status)

	rows, err := tx.Query(ctx, `SELECT name, teams_id FROM map_teams WHERE run_id = $1 ORDER BY idx`, runID)
	if err != nil {
		return Ledger{}, err
	}
	if l.Teams, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (LedgerTeam, error) {
		var t LedgerTeam
		return t, row.Scan(&t.Name, &t.TeamsID)
	}); err != nil {
		return Ledger{}, err
	}
	if l.Teams == nil {
		l.Teams = []LedgerTeam{}
	}

	rows, err = tx.Query(ctx, `
		SELECT c.slack_id, c.slack_name, c.kind, COALESCE(t.name, ''), c.target_name, c.state, c.teams_id <> '',
		       COALESCE(m.posted, 0), COALESCE(f.files, 0)
		FROM map_channels c
		LEFT JOIN map_teams t ON t.run_id = c.run_id AND t.idx = c.team_index
		LEFT JOIN (
			SELECT channel_id, count(*) AS posted
			FROM map_messages WHERE run_id = $1 AND state = 'posted' AND part = 0
			GROUP BY channel_id
		) m ON m.channel_id = c.slack_id
		LEFT JOIN (
			SELECT channel_id, count(*) AS files
			FROM map_files WHERE run_id = $1 AND state = 'uploaded'
			GROUP BY channel_id
		) f ON f.channel_id = c.slack_id
		WHERE c.run_id = $1
		ORDER BY c.slack_id COLLATE "C"`, runID)
	if err != nil {
		return Ledger{}, err
	}
	for rows.Next() {
		var (
			c     LedgerChannel
			state string
		)
		if err := rows.Scan(&c.SlackID, &c.SlackName, &c.Kind, &c.Team, &c.TargetName, &state, &c.InTeams, &c.Posted, &c.Files); err != nil {
			rows.Close()
			return Ledger{}, err
		}
		c.State = ChannelState(state)
		l.Channels = append(l.Channels, c)
		l.Posted += c.Posted
		l.Files += c.Files
	}
	if err := rows.Err(); err != nil {
		return Ledger{}, err
	}

	rows, err = tx.Query(ctx, `SELECT slack_id FROM map_users WHERE run_id = $1 ORDER BY slack_id COLLATE "C"`, runID)
	if err != nil {
		return Ledger{}, err
	}
	if l.Users, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return Ledger{}, err
	}
	if l.Users == nil {
		l.Users = []string{}
	}
	return l, nil
}

// PostedKeys calls fn for every Slack message of the channel that has a
// post in Teams, in Slack ts byte order. A message split into parts is
// reported once, by its first part, which is the one that carries the edit
// marker a later pass compares.
func (s *PG) PostedKeys(ctx context.Context, runID, channelID string, fn func(slackTS string, edited *string) error) error {
	rows, err := s.pool.Query(ctx, `
		SELECT slack_ts, source_edited FROM map_messages
		WHERE run_id = $1 AND channel_id = $2 AND part = 0 AND state = 'posted'
		ORDER BY slack_ts COLLATE "C"`, runID, channelID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ts     string
			edited *string
		)
		if err := rows.Scan(&ts, &edited); err != nil {
			return err
		}
		if err := fn(ts, edited); err != nil {
			return err
		}
	}
	return rows.Err()
}

// MessagesByTS returns every post of the given Slack messages of one
// channel, ordered by Slack ts (byte order) and part.
func (s *PG) MessagesByTS(ctx context.Context, runID, channelID string, slackTS []string) ([]Message, error) {
	if len(slackTS) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+messageCols+` FROM map_messages
		WHERE run_id = $1 AND channel_id = $2 AND slack_ts = ANY($3::text[])
		ORDER BY slack_ts COLLATE "C", part`, runID, channelID, slackTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// TakenTargets returns the allocated milliseconds of a conversation within
// [fromMS, toMS], ascending. It is answered from the unique index on
// (run, channel, target_ms).
func (s *PG) TakenTargets(ctx context.Context, runID, channelID string, fromMS, toMS int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT target_ms FROM map_messages
		WHERE run_id = $1 AND channel_id = $2 AND target_ms BETWEEN $3 AND $4
		ORDER BY target_ms`, runID, channelID, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

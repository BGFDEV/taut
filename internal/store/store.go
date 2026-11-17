// Package store is Taut's durable state: runs, the job ledger (lanes and
// batches), the Slack-to-Teams mapping tables and the audit log.
//
// Postgres is the single source of truth (see docs/design-notes.md). This
// file is the contract; the Postgres implementation lives beside it.
//
// # Lanes, leases and fencing
//
// Work is organized into lanes. A lane is an ordered list of batches that
// must be processed strictly in sequence by one holder at a time, which is
// how per-channel chronological order is guaranteed. Different lanes run in
// parallel.
//
// A worker claims a lane and receives a Lease carrying a fence token. The
// fence increases on every claim. Every write a worker makes goes through
// Store.Fenced, which runs inside one transaction that first verifies the
// lease's fence is still current (taking a lock that excludes a concurrent
// claim). A worker that lost its lease (it stalled, was partitioned, or was
// killed and replaced) therefore cannot record anything: its transaction
// fails with ErrFenced and changes nothing.
//
// A batch may depend on batches in other lanes (a load batch needs its
// transform batch). A lane is claimable only when its next pending batch has
// every dependency done.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Errors.
var (
	// ErrFenced means the lease is no longer current. The caller must stop
	// working on the lane immediately; nothing was written.
	ErrFenced = errors.New("store: lease lost (fenced)")
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrActiveRunExists means a run with the same name is still active.
	ErrActiveRunExists = errors.New("store: an active run with this name exists")
	// ErrBaseNotFinished means the run named as the base of an incremental
	// pass has not finished importing, or was abandoned.
	ErrBaseNotFinished = errors.New("store: the base run has not finished importing")
	// ErrBaseSuperseded means the run named as the base of an incremental
	// pass already has a later pass that started from it.
	ErrBaseSuperseded = errors.New("store: the base run already has a later pass")
)

// Stages, in pipeline order.
const (
	StagePlan      = "plan"      // read the export index and directory; create the ledger
	StageExtract   = "extract"   // export day file → normalized batch
	StageTransform = "transform" // normalized batch → Teams posts, timestamp allocation
	StageTeam      = "team"      // create or adopt the target team
	StageFiles     = "files"     // Slack file → staging → the channel's SharePoint folder
	StageLoad      = "load"      // create and open the channel, then import posts in order
	StageFinalize  = "finalize"  // complete a channel's migration mode (taut finalize)
	StageMembers   = "members"   // add a team's members once its channels are finalized
	// StageRelease takes a chat out of migration mode when its run will
	// not be worked to the end: it was cancelled, replaced by a new run,
	// or failed and the operator asked for its chats back. Unlike every
	// other stage it is claimable whatever the run's status.
	StageRelease = "release"
)

// RunStatus is the lifecycle of a run.
type RunStatus string

// Run statuses.
const (
	RunAwaitingExport RunStatus = "awaiting_export" // created; the export is not uploaded yet
	RunQueued         RunStatus = "queued"          // export stored; waiting to be planned
	RunRunning        RunStatus = "running"         // planned; workers are working
	RunCompleted      RunStatus = "completed"       // every lane is done
	RunFailed         RunStatus = "failed"          // at least one lane failed permanently and nothing else can progress
	RunCancelled      RunStatus = "cancelled"       // superseded by an operator
	RunFinalizing     RunStatus = "finalizing"      // import done; finalizing selected channels
	RunFinalized      RunStatus = "finalized"       // every channel finalized; the migration is over
)

// Active reports whether the run can still make progress.
func (s RunStatus) Active() bool {
	return s == RunAwaitingExport || s == RunQueued || s == RunRunning || s == RunFailed || s == RunFinalizing
}

// Run is one execution of a config file.
type Run struct {
	ID   string // opaque, unique, sortable by creation time
	Name string // the config file's metadata.name
	// Config is the YAML snapshot the run was started with.
	Config []byte
	// ConfigHash is the hex SHA-256 of config file.
	ConfigHash string
	Status     RunStatus
	// ExportKey is the staging key of the uploaded export; empty until
	// uploaded. ExportSHA256 is the hex digest of the plaintext ZIP.
	ExportKey    string
	ExportSHA256 string
	ExportBytes  int64
	// WrappedKey is the run's data-encryption key, wrapped by the
	// key-encryption key. Opaque to the store.
	WrappedKey []byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// FinishedAt is set when Status becomes completed or cancelled.
	FinishedAt *time.Time
	// Retain is set by `taut finalize --retain`: staging is kept after the run
	// is finalized instead of being purged.
	Retain bool
	// PurgedAt is set once staging has been purged and the run's data key
	// destroyed. A purged run cannot be resumed or re-read.
	PurgedAt *time.Time
	// Layout is the ledger layout the run needs (pipeline.LayoutVersion
	// and up), decided from its config when it is created. Zero means the
	// default layout.
	Layout int
	// Pass numbers the run within its migration: 1 for a first import, and
	// one more than its base for an incremental pass. Zero means 1.
	Pass int
	// BaseRunID is the run an incremental pass follows: the pass starts
	// with everything that run recorded as being in Teams. Empty for pass 1.
	BaseRunID string
	// WorkspaceID is the Slack workspace the run's export came from, as the
	// plan stage read it. Empty until planned, or when the export does not
	// say.
	WorkspaceID string
}

// Incremental reports whether the run is a later pass of a migration.
func (r Run) Incremental() bool { return r.BaseRunID != "" }

// FinalizePlan adds the finalizing work for some or all of a run's channels.
type FinalizePlan struct {
	// Lanes and Batches use StageFinalize and StageMembers. Batches may depend
	// on each other; they may not depend on batches outside the plan.
	Lanes   []Lane
	Batches []Batch
	// Retain keeps staging after the run is finalized.
	Retain bool
}

// Holder is a worker currently holding leases in a run.
type Holder struct {
	Holder string   `json:"holder"`
	Lanes  int      `json:"lanes"`
	Stages []string `json:"stages"`
	// LeaseExpires is the latest expiry among its leases.
	LeaseExpires time.Time `json:"leaseExpires"`
}

// LogCursor is a position in a run's audit log, across streams.
type LogCursor struct {
	Stream string `json:"stream"`
	Seq    int64  `json:"seq"`
}

// LogFilter narrows an audit log query. Zero fields do not filter.
type LogFilter struct {
	Stream  string // exact lane ID
	Op      string // exact operation, e.g. "message.import"
	Outcome string // "ok" or "error"
	// Contains matches a substring of the source ID, target ID or detail,
	// case-insensitively.
	Contains string
	Since    time.Time // entries at or after
	Until    time.Time // entries before
	// After resumes a query: entries after this position in (stream, seq)
	// order.
	After LogCursor
	// Limit bounds the page; zero means 200, and it is capped at 5000.
	Limit int
}

// LaneStatus is the state of a lane.
type LaneStatus string

// Lane statuses.
const (
	LaneOpen   LaneStatus = "open"   // has pending batches
	LaneDone   LaneStatus = "done"   // every batch is done
	LaneFailed LaneStatus = "failed" // a batch failed permanently; needs an operator retry
)

// Lane is an ordered queue of batches.
type Lane struct {
	RunID string
	// ID is unique within the run, e.g. "plan", "extract:C024BE91L",
	// "transform:C024BE91L", "team:0", "load:C024BE91L".
	ID    string
	Stage string
	// Key is the subject of the lane: a Slack channel ID, a team index, or
	// empty for the plan lane.
	Key    string
	Status LaneStatus
	// Holder and LeaseExpires describe the current lease, if any.
	Holder       string
	LeaseExpires *time.Time
	Fence        int64
	// Cursor is opaque lane state owned by the stage (for transform, the
	// last allocated timestamp).
	Cursor json.RawMessage
	// Attempts counts consecutive failed attempts on the current batch;
	// NotBefore delays the next claim (retry backoff).
	Attempts  int
	NotBefore time.Time
	LastError string
}

// BatchStatus is the state of a batch.
type BatchStatus string

// Batch statuses.
const (
	BatchPending BatchStatus = "pending"
	BatchDone    BatchStatus = "done"
	BatchFailed  BatchStatus = "failed" // permanent; blocks the lane until retried
)

// BatchRef names a batch.
type BatchRef struct {
	LaneID string
	Seq    int
}

// Batch is one unit of work in a lane.
type Batch struct {
	RunID  string
	LaneID string
	// Seq orders batches within the lane, starting at 0, with no gaps.
	Seq int
	// Key is the batch's subject within the lane, e.g. a day "2024-03-04",
	// or "open" for the load lane's provisioning batch.
	Key    string
	Status BatchStatus
	// Input and Output are staging keys.
	Input  string
	Output string
	// Items is the number of records the batch produced or handled.
	Items int
	// DependsOn lists batches that must be done before this one may run.
	DependsOn []BatchRef
	// Stats is free-form JSON the stage records about the batch.
	Stats     json.RawMessage
	Error     string
	UpdatedAt time.Time
}

// Lease is proof that the holder owns a lane for now.
type Lease struct {
	RunID   string
	LaneID  string
	Stage   string
	Key     string
	Holder  string
	Fence   int64
	Expires time.Time
	// Cursor is the lane's cursor as of the claim.
	Cursor json.RawMessage
	// Attempts is the lane's consecutive failed attempts at the time of
	// the claim (0 on a clean lane).
	Attempts int
}

// UserMapping is the identity decision for one Slack user, persisted so that
// every stage and every resume applies the same mapping.
type UserMapping struct {
	SlackID  string
	Name     string
	Email    string
	Method   string // "email", "override" or "unmapped"
	EntraID  string // empty when unmapped
	EntraUPN string
	Reason   string // why unmapped
}

// ChannelState is the progress of one channel's provisioning.
type ChannelState string

// Channel states.
const (
	ChannelPlanned   ChannelState = "planned"   // not created yet
	ChannelCreated   ChannelState = "created"   // exists in Teams
	ChannelMigrating ChannelState = "migrating" // in migration mode; importing
	ChannelCompleted ChannelState = "completed" // migration mode completed (taut finalize)
	// ChannelSkipped: Microsoft refused the conversation's chat (or its
	// migration mode) for a reason of its own, so it is not imported. The
	// run goes on without it. Chats only.
	ChannelSkipped ChannelState = "skipped"
)

// TeamMapping is one target team.
type TeamMapping struct {
	Index       int // position in the config file's target.teams
	Name        string
	Description string
	Visibility  string
	OwnerUPN    string
	OwnerID     string // Entra object ID
	TeamsID     string // empty until created or adopted
	// Adopted is true when an existing team was reused rather than created.
	Adopted bool
}

// NoTeam is the TeamIndex of a conversation that belongs to no team: a
// direct message or group DM, which goes into a chat.
const NoTeam = -1

// Kinds of conversation that go into a chat rather than a channel. They are
// the export's own names (slack.KindDM, slack.KindMPIM).
const (
	KindDM   = "dm"
	KindMPIM = "mpim"
)

// IsChatKind reports whether a conversation of this kind goes into a chat.
func IsChatKind(kind string) bool { return kind == KindDM || kind == KindMPIM }

// ChannelMapping is one Slack conversation and its target: a channel of a
// team or, for a direct message or group DM, a chat.
type ChannelMapping struct {
	SlackID   string
	SlackName string
	Kind      string // "public", "private", "dm" or "mpim"
	// TeamIndex is the team the channel belongs to, or NoTeam for a chat.
	TeamIndex int
	// TargetName is the channel's name in Teams. For a chat it is a label
	// made of its participants' names, used only for display.
	TargetName string
	// Members are the Slack user IDs of a chat's participants, in the order
	// the plan stage decided. Empty for a channel.
	Members []string
	// Topic is the name a group chat is created with; usually empty.
	Topic string
	// SkipCode and SkipReason say why a skipped conversation was skipped.
	SkipCode   string
	SkipReason string

	// Carried is true when the conversation was already in Teams when this
	// pass was planned: an earlier pass created it. Its TeamsID, State,
	// MigrationStart and files folder were taken over from that pass.
	Carried bool

	IsGeneral bool
	// Created is the Slack channel's creation time (zero if unknown).
	Created time.Time
	TeamsID string // empty until created or adopted; a chat's ID for a chat
	State   ChannelState
	// MigrationStart is the creation time the conversation has in Teams
	// while it is in migration mode, as Graph reported it. Nothing older
	// can be imported.
	MigrationStart *time.Time
	// FilesDriveID and FilesFolderID locate the channel's SharePoint files
	// folder (and Taut's subfolder within it) once known.
	FilesDriveID  string
	FilesFolderID string
}

// EarlierChat is the chat another run recorded for a Slack conversation.
type EarlierChat struct {
	RunID string
	// Config is that run's config file, which says which tenant it was.
	Config  []byte
	TeamsID string
	// MigrationStart is the creation time the chat had in migration mode.
	MigrationStart *time.Time
}

// IsChat reports whether the conversation goes into a chat.
func (c ChannelMapping) IsChat() bool { return IsChatKind(c.Kind) }

// FileState is the progress of one file.
type FileState string

// File states.
const (
	FilePending  FileState = "pending"  // known; nothing done yet
	FileStaged   FileState = "staged"   // downloaded from Slack into staging
	FileUploaded FileState = "uploaded" // in SharePoint; attachable
	FileSkipped  FileState = "skipped"  // not copied by design (hosted outside Slack, unavailable in the export)
	FileFailed   FileState = "failed"   // could not be downloaded or uploaded
)

// FileMapping is one Slack file in one channel and where it went. A file
// shared in two channels is two rows: each channel has its own folder.
type FileMapping struct {
	ChannelID string // Slack channel ID
	SlackID   string // Slack file ID
	Name      string
	Mimetype  string
	// Size is the size Slack reported; StagedBytes is what was actually
	// downloaded.
	Size        int64
	State       FileState
	StagingKey  string
	StagedBytes int64
	// SHA256 is the hex digest of the downloaded content.
	SHA256 string
	// DriveID, ItemID, WebDavURL and ETag identify the uploaded driveItem.
	// WebDavURL and ETag are what a Teams message needs to attach it.
	DriveID   string
	ItemID    string
	WebDavURL string
	ETag      string
	ErrorCode string
	Error     string
	// BatchSeq is the files batch the file belongs to, or CarriedBatch.
	BatchSeq int
	// Carried is true when the file was already uploaded when this pass was
	// planned.
	Carried bool
}

// MessageState is the state of one post.
type MessageState string

// Message states.
const (
	MessagePending MessageState = "pending" // timestamp allocated; not posted
	MessagePosted  MessageState = "posted"  // exists in Teams; TeamsID is set
	MessageFailed  MessageState = "failed"  // Graph rejected it permanently
	MessageHeld    MessageState = "held"    // not posted because its thread anchor failed
)

// MessageKey identifies one post. A Slack message that had to be split is
// several posts sharing SlackTS with Part 0, 1, 2…
type MessageKey struct {
	ChannelID string // Slack channel ID
	SlackTS   string // exactly as Slack wrote it; never parsed to a float
	Part      int
}

// Message is the mapping row for one post.
type Message struct {
	MessageKey
	// TargetMS is the allocated createdDateTime in Unix milliseconds. It is
	// unique per (run, channel): the store enforces this, because Graph
	// rejects duplicates and Taut relies on that for exactly-once posting.
	TargetMS int64
	// ShiftMS is how many milliseconds TargetMS was moved forward from
	// what the transform stage allocated, because a message that is not
	// Taut's already held that millisecond in the target (chats only).
	ShiftMS int64
	// AnchorTS is the Slack ts of the message this post replies to in
	// Teams, or empty for a top-level post.
	AnchorTS string
	// Hash is the hex SHA-256 of the content sent to Graph.
	Hash  string
	State MessageState
	// TeamsID is the Teams message ID once posted.
	TeamsID string
	// Adopted is true when the post was found already present after a 409
	// and its ID adopted, rather than created by this attempt.
	Adopted   bool
	ErrorCode string
	Error     string
	// BatchSeq is the transform/load batch the post belongs to, or
	// CarriedBatch for a carried post.
	BatchSeq int
	// Carried is true when the post was already in Teams when this pass
	// was planned: an earlier pass imported it.
	Carried bool
	// Edited is Slack's "edited" timestamp of the message when it was
	// transformed, a pointer to "" when it had not been edited, and nil
	// when the row predates the column and the answer is not known.
	Edited *string
}

// CarriedBatch is the BatchSeq of a carried post or file: it belongs to no
// batch of the pass that carries it.
const CarriedBatch = -1

// LogEntry is one audit log record: something Taut wrote to Microsoft 365, or
// tried to.
type LogEntry struct {
	RunID string `json:"-"`
	// Stream partitions the hash chain; entries within a stream are totally
	// ordered. Taut uses the lane ID.
	Stream string `json:"stream"`
	// Seq is assigned by the store, starting at 1 within the stream.
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	Actor string    `json:"actor"` // the worker's holder ID
	// Op is what happened, e.g. "team.create", "team.adopt",
	// "channel.create", "channel.start_migration", "message.import",
	// "message.adopt", "message.fail", "message.hold".
	Op string `json:"op"`
	// SourceID identifies the Slack object (channel ID, or channel/ts[/part]).
	SourceID string `json:"sourceId"`
	// TargetID identifies the Teams object, when one exists.
	TargetID string `json:"targetId,omitempty"`
	// ContentHash is the hex SHA-256 of what was sent, when applicable.
	ContentHash string `json:"contentHash,omitempty"`
	Outcome     string `json:"outcome"` // "ok" or "error"
	Detail      string `json:"detail,omitempty"`
	// PrevHash and EntryHash chain the stream: EntryHash is the SHA-256 of
	// PrevHash and the entry's canonical encoding. Both are set by the
	// store.
	PrevHash  string `json:"prevHash"`
	EntryHash string `json:"entryHash"`
}

// Plan is everything the plan stage decides, applied atomically.
type Plan struct {
	Users    []UserMapping
	Teams    []TeamMapping
	Channels []ChannelMapping
	// Lanes and Batches define the ledger for the rest of the run. Batches
	// reference lanes by ID and must be complete: seq 0..n-1 for each lane.
	Lanes   []Lane
	Batches []Batch
	// Totals are stored on the run for progress reporting.
	Totals json.RawMessage
	// WorkspaceID is the Slack workspace the export came from; stored on
	// the run.
	WorkspaceID string
	// CarryFrom is the run an incremental pass starts from; it must be the
	// run's BaseRunID. For every channel of the plan marked Carried, the
	// posts that run recorded as posted, its thread anchors and its
	// uploaded files are copied into this run as carried rows, in the
	// same transaction as the plan.
	CarryFrom string
}

// ClaimFilter narrows which lanes a worker will take.
type ClaimFilter struct {
	// Stages the worker can run; required.
	Stages []string
	// RunID restricts the claim to one run; empty means any active run.
	RunID string
}

// Tx is the set of writes available inside Store.Fenced. Everything done
// through one Tx commits or rolls back together.
type Tx interface {
	// --- ledger ---

	// ApplyPlan stores the plan and marks the run running. It may be called
	// only from the plan lane, once; a second call for the same run is a
	// no-op so a retried plan batch is harmless.
	ApplyPlan(ctx context.Context, p Plan) error
	// CompleteBatch marks a batch of the leased lane done. If it was the
	// lane's last batch the lane becomes done. Completing a batch that is
	// already done is a no-op.
	CompleteBatch(ctx context.Context, seq int, output string, items int, stats json.RawMessage) error
	// FailBatch marks a batch permanently failed and the lane failed.
	FailBatch(ctx context.Context, seq int, reason string) error
	// SetCursor replaces the lane's cursor.
	SetCursor(ctx context.Context, cursor json.RawMessage) error

	// --- mapping ---

	// PutMessages inserts post rows in state pending. Rows that already
	// exist (same key) are left untouched, so re-running a transform batch
	// cannot change an allocation that may already have been posted.
	PutMessages(ctx context.Context, msgs []Message) error
	// SetAnchor records that replies in the Slack thread threadTS attach
	// to the post anchorTS in this channel. First writer wins.
	SetAnchor(ctx context.Context, channelID, threadTS, anchorTS string) error
	// MarkPosted records that a post exists in Teams.
	MarkPosted(ctx context.Context, key MessageKey, teamsID string, adopted bool) error
	// MarkFailed records a permanent failure for a post.
	MarkFailed(ctx context.Context, key MessageKey, state MessageState, code, reason string) error
	// ShiftMessages moves the post at fromMS, and every post of the
	// conversation that follows it without a gap, one millisecond later.
	// It returns ErrTimestampTaken when a post in the way is already in
	// Teams, and ErrShiftLimit when one of them has been moved maxShift
	// milliseconds already; nothing is moved then.
	ShiftMessages(ctx context.Context, channelID string, fromMS, maxShift int64) (int, error)
	// SkipChannel records that a conversation is not imported, and why.
	SkipChannel(ctx context.Context, slackID, code, reason string) error
	// SetTeam records a team's Teams ID.
	SetTeam(ctx context.Context, index int, teamsID string, adopted bool) error
	// SetChannel records a channel's Teams ID and state.
	SetChannel(ctx context.Context, slackID, teamsID string, state ChannelState, migrationStart *time.Time) error
	// SetChannelFolder records the channel's SharePoint files folder.
	SetChannelFolder(ctx context.Context, slackID, driveID, folderID string) error

	// --- files ---

	// PutFiles inserts file rows. Rows that already exist (same channel
	// and Slack file ID) are left untouched.
	PutFiles(ctx context.Context, files []FileMapping) error
	// MarkFileStaged records that a file's content is in staging.
	MarkFileStaged(ctx context.Context, channelID, slackID, stagingKey, sha256 string, bytes int64) error
	// MarkFileUploaded records the driveItem a file became. Recording the
	// same item again is a no-op; a different item for an uploaded file is
	// an error.
	MarkFileUploaded(ctx context.Context, channelID, slackID, driveID, itemID, webDavURL, etag string) error
	// MarkFileSettled records a file that will not be uploaded: state is
	// FileSkipped or FileFailed. It never overwrites an uploaded file.
	MarkFileSettled(ctx context.Context, channelID, slackID string, state FileState, code, reason string) error

	// --- audit log ---

	// Log appends an entry to the lane's stream, assigning Seq and the
	// hash chain fields.
	Log(ctx context.Context, e LogEntry) error
}

// Store is the durable state.
type Store interface {
	// Migrate brings the schema up to date. Safe to call concurrently from
	// several processes.
	Migrate(ctx context.Context) error
	Close()

	// --- runs ---

	// CreateRun inserts a run. It returns ErrActiveRunExists if a run with
	// the same name has an active status. A run with BaseRunID set is an
	// incremental pass: its base must have finished importing and have no
	// other pass after it (ErrBaseNotFinished, ErrBaseSuperseded), and its
	// pass number is the base's plus one.
	CreateRun(ctx context.Context, r Run) error
	GetRun(ctx context.Context, id string) (Run, error)
	// ActiveRun returns the active run with this name, if any.
	ActiveRun(ctx context.Context, name string) (Run, bool, error)
	// LatestRun returns the most recently created run with this name.
	LatestRun(ctx context.Context, name string) (Run, bool, error)
	// SetExport records the uploaded export, creates the plan lane with its
	// single batch, and moves the run to queued. Idempotent.
	SetExport(ctx context.Context, runID, key, sha256 string, size int64) error
	// CancelRun marks a run cancelled and releases its lanes.
	CancelRun(ctx context.Context, runID string) error
	// RetryFailed returns failed batches and lanes of the run to pending/open
	// and clears backoff, and returns how many lanes were reset. If the run
	// was failed it becomes running again.
	RetryFailed(ctx context.Context, runID string) (int, error)
	// Reconcile updates run statuses from their lanes: running → completed
	// when every lane is done; running → failed when no lane is open or
	// claimable and at least one failed. A finalizing run becomes finalized when
	// its lanes are done and every channel is completed, goes back to
	// completed when its lanes are done but some channels were not
	// selected, and becomes failed like a running one. It returns the runs
	// it changed.
	Reconcile(ctx context.Context) ([]Run, error)
	// BeginFinalize adds a finalize plan to a completed (or already finalizing) run
	// and makes it finalizing. Lanes that already exist are left as they are,
	// so repeating a finalize request is harmless; a lane of the plan that
	// previously failed is reopened. It returns ErrNotFound for an unknown
	// run and an error if the run is in any other status.
	BeginFinalize(ctx context.Context, runID string, p FinalizePlan) error
	// ReleaseChats queues a release lane for each of the run's chats that
	// may be in migration mode (all of them, or those named), whatever
	// the run's status, and reopens release lanes that failed. It returns
	// how many chats now have release work that is not done.
	ReleaseChats(ctx context.Context, runID string, slackIDs []string) (int, error)
	// EarlierChats returns the chats other runs recorded for a Slack
	// conversation, newest run first.
	EarlierChats(ctx context.Context, runID, slackID string) ([]EarlierChat, error)
	// MarkPurged records that the run's staging was purged: it clears the
	// wrapped data key and sets PurgedAt. Only a finalized or cancelled
	// run, or a completed run that a later pass was planned from, can be
	// purged.
	MarkPurged(ctx context.Context, runID string) error
	// Purgeable returns the runs whose staging should be deleted: finalized
	// runs that are not retained, cancelled runs, and completed runs that a
	// later pass was planned from, in every case not yet purged and with no
	// lease still held (a batch in flight may still be writing to
	// staging). Oldest first.
	Purgeable(ctx context.Context) ([]Run, error)
	// ActiveRuns returns every run that is not over, oldest first.
	ActiveRuns(ctx context.Context) ([]Run, error)

	// --- leases ---

	// Claim leases one claimable lane to holder for ttl, or returns nil if
	// there is none. A lane is claimable when its run is active, it is
	// open, it is unleased or its lease has expired, its NotBefore has
	// passed, and its next pending batch has no unfinished dependency.
	// Claims prefer earlier stages, then older runs.
	Claim(ctx context.Context, f ClaimFilter, holder string, ttl time.Duration) (*Lease, error)
	// Renew extends a lease. It returns ErrFenced if the lease was lost.
	Renew(ctx context.Context, l *Lease, ttl time.Duration) error
	// Release gives a lane back. If retryAfter is positive the lane's
	// attempt counter is incremented, lastError recorded, and the lane is
	// not claimable again until that long has passed (a failed attempt).
	// Otherwise the attempt counter is reset (a clean hand-back).
	// Releasing a lost lease is a no-op.
	Release(ctx context.Context, l *Lease, retryAfter time.Duration, lastError string) error
	// NextBatch returns the leased lane's next pending batch if all of its
	// dependencies are done, or nil if the lane is finished or blocked.
	NextBatch(ctx context.Context, l *Lease) (*Batch, error)
	// Fenced runs fn in one transaction after verifying the lease. If the
	// lease is no longer current it returns ErrFenced without calling fn.
	// If fn returns an error the transaction rolls back and Fenced returns
	// that error.
	Fenced(ctx context.Context, l *Lease, fn func(tx Tx) error) error

	// --- reads ---

	GetBatch(ctx context.Context, runID string, ref BatchRef) (Batch, error)
	Users(ctx context.Context, runID string) ([]UserMapping, error)
	Teams(ctx context.Context, runID string) ([]TeamMapping, error)
	Channels(ctx context.Context, runID string) ([]ChannelMapping, error)
	Channel(ctx context.Context, runID, slackID string) (ChannelMapping, error)
	// MessagesInBatch returns the posts of one channel's batch, ordered by
	// TargetMS.
	MessagesInBatch(ctx context.Context, runID, channelID string, batchSeq int) ([]Message, error)
	// GetMessage returns one post, or ErrNotFound.
	GetMessage(ctx context.Context, runID string, key MessageKey) (Message, error)
	// LastTargetMS returns the latest millisecond allocated to any post of
	// a conversation, or 0 if it has none.
	LastTargetMS(ctx context.Context, runID, channelID string) (int64, error)
	// MessagesByTS returns every post (all parts) of the given Slack
	// messages of one channel, ordered by Slack ts and part. Messages the
	// ledger does not know are simply absent.
	MessagesByTS(ctx context.Context, runID, channelID string, slackTS []string) ([]Message, error)
	// TakenTargets returns the milliseconds in [fromMS, toMS] that are
	// allocated to a post of the conversation, ascending.
	TakenTargets(ctx context.Context, runID, channelID string, fromMS, toMS int64) ([]int64, error)
	// Anchor returns the anchor ts for a Slack thread, if recorded.
	Anchor(ctx context.Context, runID, channelID, threadTS string) (string, bool, error)
	// GetFile returns one file row, or ErrNotFound.
	GetFile(ctx context.Context, runID, channelID, slackID string) (FileMapping, error)
	// FilesInBatch returns the files of one channel's files batch, ordered
	// by Slack file ID.
	FilesInBatch(ctx context.Context, runID, channelID string, batchSeq int) ([]FileMapping, error)
	// Progress aggregates the run for status displays.
	Progress(ctx context.Context, runID string) (Progress, error)
	// Lanes returns the run's lanes of the given stages (all stages when
	// none is given), ordered by ID. Only ID, Stage, Key and Status are
	// filled.
	Lanes(ctx context.Context, runID string, stages ...string) ([]Lane, error)
	// Holders returns the workers holding unexpired leases in the run,
	// ordered by holder.
	Holders(ctx context.Context, runID string) ([]Holder, error)

	// --- passes ---

	// Passes returns the chain of runs that ends at runID, pass 1 first:
	// the run's base, that run's base and so on, then the run itself.
	Passes(ctx context.Context, runID string) ([]Run, error)
	// Follower returns the pass that follows runID: the run that names it
	// as its base and is not cancelled. There is at most one.
	Follower(ctx context.Context, runID string) (Run, bool, error)
	// Ledger summarizes what a run recorded as being in Teams, for the
	// preflight of the pass that would follow it.
	Ledger(ctx context.Context, runID string) (Ledger, error)
	// PostedKeys calls fn for every Slack message of the channel with a
	// post in Teams (carried ones included), in Slack ts byte order, with
	// the edit marker of its first part. It stops at fn's first error.
	PostedKeys(ctx context.Context, runID, channelID string, fn func(slackTS string, edited *string) error) error

	// --- audit log ---

	// LogEntries returns entries of a run, optionally one stream, in
	// (stream, seq) order, starting after the given seq, up to limit.
	LogEntries(ctx context.Context, runID, stream string, afterSeq int64, limit int) ([]LogEntry, error)
	// LogQuery returns a filtered page of the run's audit log in (stream, seq)
	// order and the cursor to pass as After for the next page. The cursor
	// is the zero value when there are no more entries.
	LogQuery(ctx context.Context, runID string, f LogFilter) ([]LogEntry, LogCursor, error)
	// VerifyLog recomputes every hash chain of the run and returns an error
	// naming the first stream and seq that does not verify, or a gap.
	VerifyLog(ctx context.Context, runID string) error
}

// StageProgress counts batches of one stage.
type StageProgress struct {
	Stage       string `json:"stage"`
	Lanes       int    `json:"lanes"`
	Done        int    `json:"lanesDone"`
	Failed      int    `json:"lanesFailed"`
	Leased      int    `json:"lanesActive"`
	Batches     int    `json:"batches"`
	BatchesDone int    `json:"batchesDone"`
}

// ChannelProgress is one channel's import progress.
type ChannelProgress struct {
	SlackID    string `json:"slackId"`
	SlackName  string `json:"slackName"`
	TargetName string `json:"targetName"`
	// Kind is "public", "private", "dm" or "mpim".
	Kind string `json:"kind"`
	// SkipCode says why a conversation in state "skipped" was skipped.
	SkipCode string `json:"skipCode,omitempty"`
	// Team is the name of the channel's team; empty for a chat.
	Team  string       `json:"team"`
	State ChannelState `json:"state"`
	// Posts is the number of posts allocated so far (it grows as transform
	// proceeds); Posted, Failed and Held partition the finished ones.
	// Posts an earlier pass imported are not among them; they are Carried.
	Posts  int `json:"posts"`
	Posted int `json:"posted"`
	Failed int `json:"failed"`
	Held   int `json:"held"`
	// Carried is the number of posts already in Teams when this pass was
	// planned.
	Carried int `json:"carried,omitempty"`
	// Files counts the channel's file rows; FilesDone those uploaded,
	// skipped or failed. Carried files are not counted.
	Files     int `json:"files"`
	FilesDone int `json:"filesDone"`
	// Earlier is true when an earlier pass created the conversation.
	Earlier bool `json:"earlier,omitempty"`
	// Active is true while a worker holds the channel's load lane.
	Active bool `json:"active"`
	// LaneFailed is true when the channel's load lane failed.
	LaneFailed bool   `json:"laneFailed"`
	LastError  string `json:"lastError,omitempty"`
}

// LaneProblem is a failed or retrying lane, for the error feed.
type LaneProblem struct {
	LaneID    string    `json:"lane"`
	Stage     string    `json:"stage"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	NotBefore time.Time `json:"notBefore"`
	Error     string    `json:"error"`
}

// Progress is a point-in-time summary of a run.
type Progress struct {
	Run      Run               `json:"-"`
	Stages   []StageProgress   `json:"stages"`
	Channels []ChannelProgress `json:"channels"`
	Problems []LaneProblem     `json:"problems"`
	// Posts totals across channels.
	Posts   int `json:"posts"`
	Posted  int `json:"posted"`
	Failed  int `json:"failed"`
	Held    int `json:"held"`
	Adopted int `json:"adopted"`
	// Carried is the number of posts earlier passes imported, and
	// CarriedFiles the files they uploaded. Neither is part of the other
	// totals.
	Carried      int `json:"carried,omitempty"`
	CarriedFiles int `json:"carriedFiles,omitempty"`
	// Files totals across channels. FileBytes is the sum of reported sizes
	// of all files; FileBytesDone the sum for uploaded ones.
	Files         int   `json:"files"`
	FilesUploaded int   `json:"filesUploaded"`
	FilesSkipped  int   `json:"filesSkipped"`
	FilesFailed   int   `json:"filesFailed"`
	FileBytes     int64 `json:"fileBytes"`
	FileBytesDone int64 `json:"fileBytesDone"`
	// Totals is the JSON the plan stage stored.
	Totals json.RawMessage `json:"totals,omitempty"`
}

package worker

import "context"

// BatchInfo identifies one attempt at one batch, for telemetry.
type BatchInfo struct {
	RunID   string
	Lane    string
	Stage   string
	Seq     int
	Holder  string
	Attempt int
}

// Event is something a worker counts.
type Event string

// Events reported through Observer.Count.
const (
	EventPostImported     Event = "post.imported" // created in Teams by this attempt
	EventPostAdopted      Event = "post.adopted"  // found already in Teams and adopted
	EventPostRejected     Event = "post.rejected" // refused by Graph for its content
	EventPostHeld         Event = "post.held"     // not sent: its parent is missing
	EventFileUploaded     Event = "file.uploaded"
	EventFileSkipped      Event = "file.skipped"
	EventFileFailed       Event = "file.failed"
	EventFileBytes        Event = "file.bytes" // bytes uploaded to SharePoint
	EventLeaseLost        Event = "lease.lost" // fenced out or expired mid-batch
	EventBatchRetried     Event = "batch.retried"
	EventChannelFinalized Event = "channel.finalized"
	EventMemberAdded      Event = "member.added"
)

// Observer receives telemetry from a worker. Implementations must be safe
// for concurrent use and must never block the work: telemetry is best
// effort. A nil Config.Observer means none.
type Observer interface {
	// Batch is called when work on a batch starts. It returns the context
	// to do the work under (so that outbound requests join the batch's
	// trace) and a function to call with the outcome; nil means success.
	Batch(ctx context.Context, b BatchInfo) (context.Context, func(err error))
	// Count adds n occurrences of an event. stage is the stage reporting
	// it.
	Count(ctx context.Context, stage string, e Event, n int64)
}

type noObserver struct{}

func (noObserver) Batch(ctx context.Context, _ BatchInfo) (context.Context, func(error)) {
	return ctx, func(error) {}
}
func (noObserver) Count(context.Context, string, Event, int64) {}

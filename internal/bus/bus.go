// Package bus carries wake-up signals between the controller and the workers.
//
// The bus is a hint, never a source of truth. A signal says "there may be
// work"; the worker then asks Postgres what is actually claimable. Signals
// can therefore be lost, duplicated or coalesced without affecting
// correctness: every worker also polls on a timer. This is what lets
// Postgres stay the only authority (see docs/design-notes.md) while idle
// workers still react within milliseconds.
package bus

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/bgfdev/taut/internal/fail"
)

// Subject is the single subject wake-ups are published on.
const Subject = "taut.wake"

// Bus publishes and receives wake-ups.
type Bus interface {
	// Wake tells every subscriber that there may be new work. It never
	// blocks on slow subscribers and never fails the caller's work: an
	// error means only that the hint was not delivered.
	Wake(ctx context.Context) error
	// Subscribe returns a channel that receives a value after each wake-up.
	// Bursts are coalesced: the channel holds at most one pending signal.
	// Call the returned function to stop.
	Subscribe() (<-chan struct{}, func())

	// Publish sends data to every listener on subject. Like Wake it is
	// best-effort: a listener that is not keeping up misses messages.
	Publish(ctx context.Context, subject string, data []byte) error
	// Listen returns a channel of messages published on subject. The
	// channel is buffered; when it is full, new messages are dropped rather
	// than blocking the publisher. Call the returned function to stop.
	Listen(subject string) (<-chan []byte, func())
	// Request sends data to whoever serves subject and waits for the reply.
	// It returns ErrNoResponder if nobody serves it or the reply does not
	// arrive before ctx is done.
	Request(ctx context.Context, subject string, data []byte) ([]byte, error)
	// Serve answers requests on subject. Call the returned function to
	// stop.
	Serve(subject string, handler func(data []byte) []byte) (func(), error)
	Close()
}

// ErrNoResponder means a Request found nobody to answer it in time.
var ErrNoResponder = errors.New("bus: no responder")

// listenBuffer is how many undelivered messages a listener may hold.
const listenBuffer = 64

// --- in-process ---------------------------------------------------------------

// Local is an in-process Bus, for single-process runs and tests.
type Local struct {
	mu        sync.Mutex
	subs      map[chan struct{}]struct{}
	listeners map[string]map[chan []byte]struct{}
	servers   map[string]func([]byte) []byte
}

// NewLocal returns an in-process Bus.
func NewLocal() *Local {
	return &Local{
		subs:      map[chan struct{}]struct{}{},
		listeners: map[string]map[chan []byte]struct{}{},
		servers:   map[string]func([]byte) []byte{},
	}
}

// Publish implements Bus.
func (l *Local) Publish(_ context.Context, subject string, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ch := range l.listeners[subject] {
		select {
		case ch <- data:
		default:
		}
	}
	return nil
}

// Listen implements Bus.
func (l *Local) Listen(subject string) (<-chan []byte, func()) {
	ch := make(chan []byte, listenBuffer)
	l.mu.Lock()
	if l.listeners[subject] == nil {
		l.listeners[subject] = map[chan []byte]struct{}{}
	}
	l.listeners[subject][ch] = struct{}{}
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		delete(l.listeners[subject], ch)
		l.mu.Unlock()
	}
}

// Request implements Bus.
func (l *Local) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	l.mu.Lock()
	h := l.servers[subject]
	l.mu.Unlock()
	if h == nil {
		return nil, ErrNoResponder
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return h(data), nil
}

// Serve implements Bus. A subject has one server; serving it again replaces
// the previous one.
func (l *Local) Serve(subject string, handler func([]byte) []byte) (func(), error) {
	l.mu.Lock()
	l.servers[subject] = handler
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.servers, subject)
		l.mu.Unlock()
	}, nil
}

// Wake implements Bus.
func (l *Local) Wake(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ch := range l.subs {
		select {
		case ch <- struct{}{}:
		default: // a signal is already pending; that is enough
		}
	}
	return nil
}

// Subscribe implements Bus.
func (l *Local) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		delete(l.subs, ch)
		l.mu.Unlock()
	}
}

// Close implements Bus.
func (l *Local) Close() {}

// --- NATS ---------------------------------------------------------------------

// NATS is a Bus over core NATS publish/subscribe. It reconnects on its own;
// while disconnected, workers fall back to their polling timer.
type NATS struct {
	nc    *nats.Conn
	local *Local
	sub   *nats.Subscription
}

// NATSOptions are how a process identifies itself to NATS and what it
// trusts. All are optional file paths.
type NATSOptions struct {
	// CredsFile is a NATS credentials file (user JWT and seed).
	CredsFile string
	// CAFile is a PEM bundle the server's certificate must chain to.
	CAFile string
	// CertFile and KeyFile are a client certificate, for servers that
	// verify clients.
	CertFile, KeyFile string
}

// DialNATS connects to a NATS server.
func DialNATS(url, clientName string, opts NATSOptions) (*NATS, error) {
	o := []nats.Option{
		nats.Name(clientName),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.RetryOnFailedConnect(true),
		nats.Timeout(3 * time.Second),
	}
	if opts.CredsFile != "" {
		o = append(o, nats.UserCredentials(opts.CredsFile))
	}
	if opts.CAFile != "" {
		o = append(o, nats.RootCAs(opts.CAFile))
	}
	if opts.CertFile != "" || opts.KeyFile != "" {
		o = append(o, nats.ClientCert(opts.CertFile, opts.KeyFile))
	}
	nc, err := nats.Connect(url, o...)
	if err != nil {
		return nil, fail.Wrap(err, "bus.unreachable", "Could not connect to NATS", "",
			"Check TAUT_NATS_URL, the credentials and certificate files if any are set, and that the NATS server is running (`make up` starts one).")
	}
	n := &NATS{nc: nc, local: NewLocal()}
	// One NATS subscription fans out to local subscribers, so a process
	// holds a single server-side interest however many loops it runs.
	n.sub, err = nc.Subscribe(Subject, func(*nats.Msg) { _ = n.local.Wake(context.Background()) })
	if err != nil {
		nc.Close()
		return nil, fail.Wrap(err, "bus.subscribe_failed", "Could not subscribe to wake-ups on NATS", "",
			"Check the NATS account's permissions for the subject "+Subject+".")
	}
	return n, nil
}

// Wake implements Bus.
func (n *NATS) Wake(context.Context) error { return n.nc.Publish(Subject, nil) }

// Subscribe implements Bus.
func (n *NATS) Subscribe() (<-chan struct{}, func()) { return n.local.Subscribe() }

// Close implements Bus.
func (n *NATS) Close() {
	_ = n.sub.Unsubscribe()
	n.nc.Close()
}

// Publish implements Bus.
func (n *NATS) Publish(_ context.Context, subject string, data []byte) error {
	return n.nc.Publish(subject, data)
}

// Listen implements Bus.
func (n *NATS) Listen(subject string) (<-chan []byte, func()) {
	ch := make(chan []byte, listenBuffer)
	sub, err := n.nc.Subscribe(subject, func(m *nats.Msg) {
		select {
		case ch <- m.Data:
		default:
		}
	})
	if err != nil {
		// A listener that could not subscribe hears nothing; the
		// callers of Listen all tolerate missed messages.
		return ch, func() {}
	}
	return ch, func() { _ = sub.Unsubscribe() }
}

// Request implements Bus.
func (n *NATS) Request(ctx context.Context, subject string, data []byte) ([]byte, error) {
	m, err := n.nc.RequestWithContext(ctx, subject, data)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) {
			return nil, ErrNoResponder
		}
		return nil, err
	}
	return m.Data, nil
}

// Serve implements Bus. Servers of one subject share a queue group, so with
// several controller replicas each request is answered once.
func (n *NATS) Serve(subject string, handler func([]byte) []byte) (func(), error) {
	sub, err := n.nc.QueueSubscribe(subject, "taut", func(m *nats.Msg) {
		_ = m.Respond(handler(m.Data))
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/bgfdev/taut/internal/store"
)

// fencedOnly is a ledger of which only Fenced is ever called.
type fencedOnly struct {
	store.Store
	err error
}

func (f fencedOnly) Fenced(context.Context, *store.Lease, func(store.Tx) error) error { return f.err }

// Every fenced transaction is timed, by stage and outcome, and nothing that
// identifies a run, a lane or a worker reaches the metric.
func TestLedgerTransactionsAreTimed(t *testing.T) {
	h := newHarness(t)
	lease := &store.Lease{RunID: "run-01J", LaneID: "load:C024BE91L", Stage: "load", Key: "C024BE91L", Holder: "host-12-ab"}
	ok := h.Ledger(fencedOnly{})
	for range 3 {
		if err := ok.Fenced(context.Background(), lease, nil); err != nil {
			t.Fatal(err)
		}
	}
	failing := h.Ledger(fencedOnly{err: errors.New("lease lost for run-01J")})
	if err := failing.Fenced(context.Background(), lease, nil); err == nil {
		t.Fatal("the error was swallowed")
	}
	_ = ok.Fenced(context.Background(), &store.Lease{Stage: "Not A Stage"}, nil)

	rm := h.collect(t)
	const name = "taut.ledger.transaction.duration"
	stage := func(s string) attribute.KeyValue { return attribute.String("taut.stage", s) }
	outcome := func(s string) attribute.KeyValue { return attribute.String("outcome", s) }
	if n := histogramCount(rm, name, stage("load"), outcome("ok")); n != 3 {
		t.Errorf("load/ok count = %d, want 3", n)
	}
	if n := histogramCount(rm, name, stage("load"), outcome("error")); n != 1 {
		t.Errorf("load/error count = %d, want 1", n)
	}
	if n := histogramCount(rm, name, stage("other"), outcome("ok")); n != 1 {
		t.Errorf("a stage that is not a label was not folded into other (%d)", n)
	}
	dump := dumpMetrics(t, rm)
	for _, id := range []string{"run-01J", "C024BE91L", "host-12-ab"} {
		if strings.Contains(dump, id) {
			t.Errorf("metrics contain the identifier %q", id)
		}
	}

	// Off: the ledger is handed back as it is.
	var off *Providers
	plain := fencedOnly{}
	if got := off.Ledger(plain); got != store.Store(plain) {
		t.Error("with telemetry off the ledger was wrapped")
	}
}

// A request of a few milliseconds and one of a few hundred fall into
// different buckets: the HTTP histograms are in seconds, and their
// boundaries must be too, or no percentile can be read from them.
func TestHTTPDurationBucketsAreInSeconds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.http.client.Record(ctx, 0.004)
	h.http.client.Record(ctx, 0.3)
	h.http.server.Record(ctx, 0.004)
	rm := h.collect(t)
	for _, name := range []string{"http.client.request.duration", "http.server.request.duration"} {
		found := false
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				d, ok := m.Data.(metricdata.Histogram[float64])
				if !ok || m.Name != name {
					continue
				}
				found = true
				for _, p := range d.DataPoints {
					if len(p.Bounds) < 10 || p.Bounds[0] > 0.005 || p.Bounds[len(p.Bounds)-1] < 10 {
						t.Errorf("%s boundaries = %v", name, p.Bounds)
					}
					filled := 0
					for _, c := range p.BucketCounts {
						if c > 0 {
							filled++
						}
					}
					if name == "http.client.request.duration" && filled != 2 {
						t.Errorf("%s: 4 ms and 300 ms share a bucket (%v over %v)", name, p.BucketCounts, p.Bounds)
					}
				}
			}
		}
		if !found {
			t.Errorf("no %s histogram", name)
		}
	}
}

package fixtures_test

import (
	"io"
	"runtime"
	"testing"

	"github.com/bgfdev/taut/internal/fixtures"
)

// BenchmarkLoadFixture reports how fast the load fixture is written and how
// much memory that takes at most, which must not grow with its size.
func BenchmarkLoadFixture(b *testing.B) {
	s, err := fixtures.ParseLoadSpec("load:channels=150,messages=200000")
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		facts, err := fixtures.WriteLoad(s, io.Discard)
		if err != nil {
			b.Fatal(err)
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		b.ReportMetric(float64(facts.Messages), "messages")
		b.ReportMetric(float64(m.HeapSys)/(1<<20), "heap-MiB")
	}
}

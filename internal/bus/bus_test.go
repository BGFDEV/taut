package bus

import (
	"context"
	"os"
	"testing"
	"time"
)

func expect(t *testing.T, ch <-chan struct{}, want bool) {
	t.Helper()
	select {
	case <-ch:
		if !want {
			t.Fatal("unexpected wake-up")
		}
	case <-time.After(500 * time.Millisecond):
		if want {
			t.Fatal("no wake-up arrived")
		}
	}
}

func exercise(t *testing.T, publisher, subscriber Bus) {
	t.Helper()
	a, stopA := subscriber.Subscribe()
	b, stopB := subscriber.Subscribe()
	defer stopB()

	// A burst coalesces to a single pending signal per subscriber.
	for range 10 {
		if err := publisher.Wake(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	expect(t, a, true)
	expect(t, b, true)
	time.Sleep(50 * time.Millisecond)
	select {
	case <-a:
	default:
	}
	expect(t, a, false)

	// After unsubscribing, a receives nothing and b still does.
	stopA()
	if err := publisher.Wake(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(t, a, false)
	expect(t, b, true)
}

func TestLocal(t *testing.T) {
	l := NewLocal()
	exercise(t, l, l)
	messaging(t, l, l)
}

// messaging checks publish/listen and request/serve.
func messaging(t *testing.T, a, b Bus) {
	t.Helper()
	ctx := context.Background()
	got, stop := b.Listen("taut.test.topic")
	defer stop()
	time.Sleep(100 * time.Millisecond) // let a remote subscription register
	if err := a.Publish(ctx, "taut.test.topic", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if string(m) != "hello" {
			t.Fatalf("got %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("published message did not arrive")
	}

	rctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	if _, err := a.Request(rctx, "taut.test.rpc", []byte("x")); err != ErrNoResponder {
		t.Fatalf("request with no server: err = %v, want ErrNoResponder", err)
	}
	cancel()

	stopServe, err := b.Serve("taut.test.rpc", func(d []byte) []byte { return append([]byte("re:"), d...) })
	if err != nil {
		t.Fatal(err)
	}
	defer stopServe()
	time.Sleep(100 * time.Millisecond)
	rctx, cancel = context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	reply, err := a.Request(rctx, "taut.test.rpc", []byte("ping"))
	if err != nil || string(reply) != "re:ping" {
		t.Fatalf("reply = %q, err = %v", reply, err)
	}
}

func TestNATS(t *testing.T) {
	url := os.Getenv("TAUT_TEST_NATS_URL")
	if url == "" {
		t.Skip("TAUT_TEST_NATS_URL is not set; run `make test-services`")
	}
	pub, err := DialNATS(url, "taut-test-pub", NATSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	sub, err := DialNATS(url, "taut-test-sub", NATSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := sub.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	exercise(t, pub, sub)
	messaging(t, pub, sub)
}

package oteljetstream

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// fakeRawBatch is a minimal jetstream.MessageBatch whose Messages() channel is
// never written to, simulating a batch parked waiting for the server to
// deliver (or time out) with no message ever arriving.
type fakeRawBatch struct {
	ch  chan jetstream.Msg
	err error
}

func newFakeRawBatch() *fakeRawBatch {
	return &fakeRawBatch{ch: make(chan jetstream.Msg)}
}

func (f *fakeRawBatch) Messages() <-chan jetstream.Msg { return f.ch }
func (f *fakeRawBatch) Error() error                   { return f.err }

// TestDirectMessageBatch_StopWhileParkedOnEmptyReceive verifies Stop() unblocks
// the forwarding goroutine promptly even while it is parked waiting to receive
// from the native batch (no message has arrived), not just while blocked
// sending to the wrapper channel.
func TestDirectMessageBatch_StopWhileParkedOnEmptyReceive(t *testing.T) {
	raw := newFakeRawBatch()
	batch := newDirectMessageBatch(raw)

	done := make(chan struct{})
	go func() {
		batch.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop() did not return promptly")
	}

	select {
	case _, ok := <-batch.Messages():
		if ok {
			t.Fatal("Messages() channel should be closed after Stop(), not yield a message")
		}
	case <-time.After(time.Second):
		t.Fatal("Messages() channel did not close promptly after Stop()")
	}
}

// TestDirectMessageBatch_FullDrain verifies the existing full-drain behavior:
// closing the native channel closes the wrapper channel without an explicit
// Stop() call.
func TestDirectMessageBatch_FullDrain(t *testing.T) {
	raw := newFakeRawBatch()
	batch := newDirectMessageBatch(raw)
	close(raw.ch)

	select {
	case _, ok := <-batch.Messages():
		if ok {
			t.Fatal("expected closed channel with no messages")
		}
	case <-time.After(time.Second):
		t.Fatal("Messages() channel did not close after native channel closed")
	}
}

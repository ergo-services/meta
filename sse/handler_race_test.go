package sse

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/testing/stage"
)

// nopFlusher stands in for the http.Flusher a serverConnection flushes to.
type nopFlusher struct{}

func (nopFlusher) Flush() {}

func factoryRaceHost() gen.ProcessBehavior { return &raceHost{} }

// raceHost spawns a serverConnection as a meta, floods it with events, then
// cancels the request context so the shutdown path runs concurrently with the
// in-flight writes. It signals disconnect back to the test over a channel.
type raceHost struct {
	act.Actor

	conn   *serverConnection
	cancel context.CancelFunc
	events int
	done   chan struct{}
}

func (h *raceHost) Init(args ...any) error {
	h.conn = args[0].(*serverConnection)
	h.cancel = args[1].(context.CancelFunc)
	h.events = args[2].(int)
	h.done = args[3].(chan struct{})

	id, err := h.SpawnMeta(h.conn, gen.MetaOptions{})
	if err != nil {
		return err
	}
	for i := 0; i < h.events; i++ {
		if err := h.SendAlias(id, Message{Event: "e", Data: []byte("some-sse-payload-data")}); err != nil {
			return err
		}
	}
	// disconnect while the mailbox is still draining the events above
	h.cancel()
	return nil
}

func (h *raceHost) HandleMessage(from gen.PID, message any) error {
	if _, ok := message.(MessageDisconnect); ok {
		close(h.done)
	}
	return nil
}

// TestServerConnectionCompressedShutdownRace drives a compressed SSE connection
// through the disconnect path while events are still being written, in the live
// runtime where Start() and HandleMessage() run in separate goroutines. The gzip
// writer is not goroutine-safe, so a Close/Flush issued off the mailbox goroutine
// corrupts its huffman tables (boundsError). Two guards: -race catches any second
// goroutine touching the writer; the gzip stream must decompress cleanly, proving
// Close ran on the mailbox path and the stream was finalized.
func TestServerConnectionCompressedShutdownRace(t *testing.T) {
	st := stage.New(t)
	n := st.StartNode("sse")

	const iterations = 64
	const events = 32

	for i := 0; i < iterations; i++ {
		sink := &bytes.Buffer{}
		gz, err := gzip.NewWriterLevel(sink, gzip.BestSpeed)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest("GET", "/sse", nil).WithContext(ctx)
		conn := &serverConnection{
			writer:     gz,
			rawFlusher: nopFlusher{},
			gzWriter:   gz,
			request:    req,
			done:       make(chan struct{}),
		}

		done := make(chan struct{})
		n.Spawn(factoryRaceHost, gen.ProcessOptions{}, conn, cancel, events, done)

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatalf("iteration %d: timed out waiting for disconnect", i)
		}

		gr, err := gzip.NewReader(bytes.NewReader(sink.Bytes()))
		if err != nil {
			t.Fatalf("iteration %d: gzip stream not finalized (Close off the mailbox path?): %s", i, err)
		}
		out, err := io.ReadAll(gr)
		if err != nil {
			t.Fatalf("iteration %d: gzip decode: %s", i, err)
		}
		if bytes.Contains(out, []byte(": connected")) == false {
			t.Fatalf("iteration %d: initial heartbeat missing from decoded stream", i)
		}
	}
}

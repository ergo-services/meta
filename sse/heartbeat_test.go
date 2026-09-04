package sse

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/testing/mock"
	"ergo.services/ergo/testing/stage"
)

// Accept is a list: a client that reads more than one type still gets the stream.
func TestHandlerAcceptsMediaTypeList(t *testing.T) {
	cases := map[string]bool{
		"":                                    true,
		"text/event-stream":                   true,
		"application/json, text/event-stream": true,
		"text/event-stream;q=0.9":             true,
		"text/event-stream, */*":              true,
		"*/*":                                 true,
		"TEXT/EVENT-STREAM":                   true,
		"text/*":                              true,
		"text/html":                           false,
		"application/json":                    false,
	}

	for accept, acceptable := range cases {
		if acceptsStream(accept) != acceptable {
			t.Errorf("Accept %q came out as acceptable=%v", accept, acceptsStream(accept))
		}
	}

	// and the refusal is wired to the handler: 406 plus its counter
	h := CreateHandler(HandlerOptions{}).(*handler)
	if err := h.Init(mock.NewMeta()); err != nil {
		t.Fatalf("init: %s", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/sse", nil)
	request.Header.Set("Accept", "text/html")
	h.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotAcceptable {
		t.Errorf("a client that cannot read the stream got %d", recorder.Code)
	}
	if h.HandleInspect(gen.PID{})["not_acceptable"] != "1" {
		t.Error("the refusal was not counted")
	}
}

// syncBuffer is the sink the meta writes into while the test reads it.
type syncBuffer struct {
	mutex sync.Mutex
	buf   bytes.Buffer
}

func (s *syncBuffer) Write(b []byte) (int, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.buf.Write(b)
}

func (s *syncBuffer) String() string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.buf.String()
}

func factoryBeatHost() gen.ProcessBehavior { return &beatHost{} }

// beatHost spawns a connection as its meta and keeps the alias, so the test can push
// events into the live stream.
type beatHost struct {
	act.Actor
	alias gen.Alias
}

func (h *beatHost) Init(args ...any) error {
	conn := args[0].(*serverConnection)
	alias, err := h.SpawnMeta(conn, gen.MetaOptions{})
	if err != nil {
		return err
	}
	h.alias = alias
	if ready, ok := args[1].(chan gen.Alias); ok {
		ready <- alias
	}
	return nil
}

// beatStand starts a live connection with the given heartbeat and returns its sink.
func beatStand(t *testing.T, heartbeat time.Duration, event string) (*syncBuffer, *serverConnection, gen.Alias, *stage.Node, context.CancelFunc) {
	t.Helper()

	st := stage.New(t)
	node := st.StartNode("sse")

	sink := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	conn := &serverConnection{
		writer:     sink,
		rawFlusher: nopFlusher{},
		request:    httptest.NewRequest("GET", "/sse", nil).WithContext(ctx),
		heartbeat:  heartbeat,
		beat:       defaultBeat,
		beatEvent:  event,
		done:       make(chan struct{}),
	}

	ready := make(chan gen.Alias, 1)
	if _, err := node.Native().Spawn(factoryBeatHost, gen.ProcessOptions{}, conn, ready); err != nil {
		t.Fatalf("spawn host: %s", err)
	}

	var alias gen.Alias
	select {
	case alias = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection was not spawned")
	}
	return sink, conn, alias, node, cancel
}

func beats(t *testing.T, node *stage.Node, alias gen.Alias) string {
	t.Helper()

	state, err := node.Native().InspectMeta(alias)
	if err != nil {
		t.Fatalf("inspect connection: %s", err)
	}
	return state["heartbeats"]
}

// A quiet stream is kept open by comment lines, and the count is visible in inspect.
func TestHeartbeatKeepsAQuietStreamOpen(t *testing.T) {
	sink, _, alias, node, _ := beatStand(t, 150*time.Millisecond, "")

	deadline := time.After(5 * time.Second)
	for {
		if strings.Count(sink.String(), ":\r\n") >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no comment lines within the deadline: %q", sink.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if beats(t, node, alias) == "0" {
		t.Error("the connection does not report what it wrote")
	}
}

// A stream that is sending data pays nothing for the heartbeat.
func TestHeartbeatSkipsABusyStream(t *testing.T) {
	sink, _, alias, node, _ := beatStand(t, 300*time.Millisecond, "")

	stop := time.After(time.Second)
	for done := false; done == false; {
		select {
		case <-stop:
			done = true
		case <-time.After(50 * time.Millisecond):
			if err := node.Native().Send(alias, Message{Event: "e", Data: []byte("x")}); err != nil {
				t.Fatalf("send: %s", err)
			}
		}
	}

	if strings.Contains(sink.String(), ":\r\n") {
		t.Errorf("a busy stream got a comment line: %q", sink.String())
	}
	if beats(t, node, alias) != "0" {
		t.Errorf("heartbeats came out as %s", beats(t, node, alias))
	}
}

// Negative disables it: the option has to be able to say "never".
func TestHeartbeatDisabled(t *testing.T) {
	sink, _, alias, node, _ := beatStand(t, -1, "")

	time.Sleep(500 * time.Millisecond)

	if strings.Contains(sink.String(), ":\r\n") {
		t.Errorf("a disabled heartbeat wrote a comment: %q", sink.String())
	}
	if beats(t, node, alias) != "0" {
		t.Errorf("heartbeats came out as %s", beats(t, node, alias))
	}
	if strings.Contains(sink.String(), ": connected") == false {
		t.Error("the stream was not established at all")
	}
}

// A name turns the keepalive into an event; without one it stays a comment.
func TestHeartbeatEvent(t *testing.T) {
	h := CreateHandler(HandlerOptions{HeartbeatEvent: "heartbeat"}).(*handler)
	if err := h.Init(mock.NewMeta()); err != nil {
		t.Fatalf("init: %s", err)
	}
	if h.beatEvent != "heartbeat" {
		t.Errorf("the beat event came out as %q", h.beatEvent)
	}

	// a bare colon is what a stream gets when nothing is configured
	plain := CreateHandler(HandlerOptions{}).(*handler)
	if string(plain.beat) != ":\r\n" || plain.beatEvent != "" {
		t.Errorf("the default beat came out as %q / %q", plain.beat, plain.beatEvent)
	}

	// a name that could inject a frame does not start
	broken := CreateHandler(HandlerOptions{HeartbeatEvent: "x\r\ndata: injected"})
	if err := broken.Init(mock.NewMeta()); err == nil {
		t.Error("a name spanning lines was accepted")
	}
}

// The event reaches the wire named and carrying the clock. Data matters: an event with
// none is never dispatched, so a client would see nothing.
func TestHeartbeatEventOnTheWire(t *testing.T) {
	sink, _, _, _, _ := beatStand(t, 150*time.Millisecond, "heartbeat")

	deadline := time.After(5 * time.Second)
	for {
		out := sink.String()
		if index := strings.Index(out, "event: heartbeat\n"); index >= 0 {
			rest := out[index:]
			end := strings.Index(rest, "\n\n")
			if end < 0 {
				continue // the frame is still being written
			}
			frame := rest[:end]
			data, found := strings.CutPrefix(frame, "event: heartbeat\ndata: ")
			if found == false {
				t.Fatalf("the beat frame came out as %q", frame)
			}
			millis, err := strconv.ParseInt(data, 10, 64)
			if err != nil {
				t.Fatalf("the beat carries %q, not a clock: %s", data, err)
			}
			if millis <= 0 {
				t.Errorf("the beat carries %d", millis)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no heartbeat event: %q", sink.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

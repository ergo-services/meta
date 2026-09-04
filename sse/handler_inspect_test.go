package sse

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ergo.services/ergo/gen"
	"ergo.services/ergo/testing/mock"
)

// plainWriter is an http.ResponseWriter that cannot stream: no Flush.
type plainWriter struct {
	header http.Header
	status int
}

func (w *plainWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *plainWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *plainWriter) WriteHeader(status int)      { w.status = status }

// Every reason a stream is turned away is counted, and the summary says which.
func TestHandlerInspect(t *testing.T) {
	h := CreateHandler(HandlerOptions{
		ProcessPool: []gen.Atom{"worker_one", "worker_two"},
		Compression: true,
	}).(*handler)

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest("GET", "/sse", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("before Init the handler answered %d", recorder.Code)
	}

	insp := h.HandleInspect(gen.PID{})
	if insp["state"] != "not initialized" || insp["unavailable"] != "1" {
		t.Fatalf("inspect before Init = %v", insp)
	}
	if insp["pool"] != "worker_one,worker_two" || insp["compression"] != "true" {
		t.Fatalf("the options are not reported: %v", insp)
	}
	if insp["heartbeat"] != defaultHeartbeat.String() {
		t.Errorf("the heartbeat came out as %q", insp["heartbeat"])
	}
	if insp["connections"] != "0" || insp["open"] != "0" || insp["last_connect"] != "never" {
		t.Errorf("a handler that served nothing = %v", insp)
	}

	if err := h.Init(mock.NewMeta()); err != nil {
		t.Fatalf("init: %s", err)
	}
	if insp := h.HandleInspect(gen.PID{}); insp["state"] != "running" {
		t.Fatalf("inspect after Init = %v", insp)
	}

	request := httptest.NewRequest("GET", "/sse", nil)
	request.Header.Set("Accept", "text/html")
	h.ServeHTTP(httptest.NewRecorder(), request)

	h.ServeHTTP(&plainWriter{}, httptest.NewRequest("GET", "/sse", nil))

	insp = h.HandleInspect(gen.PID{})
	if insp["not_acceptable"] != "1" {
		t.Errorf("a client that does not take the stream was not counted: %v", insp)
	}
	if insp["no_flusher"] != "1" {
		t.Errorf("a writer that cannot stream was not counted: %v", insp)
	}
	if insp["connections"] != "0" {
		t.Errorf("a refused request was counted as a connection: %v", insp)
	}

	queried := h.HandleInspect(gen.PID{}, "help", "nonsense")
	if queried["help"] == "" || queried["nonsense"] != "<unknown item>" {
		t.Errorf("queries came out as %v", queried)
	}
}

// A connection reports its stream: where it goes, how much went out, and when.
func TestServerConnectionInspect(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/sse", nil)
	request.Header.Set("Last-Event-ID", "42")

	c := &serverConnection{
		writer:     recorder,
		rawFlusher: recorder,
		process:    "worker_one",
		request:    request,
		heartbeat:  defaultHeartbeat,
		done:       make(chan struct{}),
	}

	insp := c.HandleInspect(gen.PID{})
	if insp["state"] != "streaming" || insp["uptime"] != "never started" {
		t.Fatalf("inspect before Init = %v", insp)
	}
	if insp["process"] != "worker_one" {
		t.Errorf("the target came out as %q, quoted or wrong", insp["process"])
	}
	if insp["messages"] != "0" || insp["bytes_out"] != "0" || insp["last_message"] != "never" {
		t.Errorf("a connection that wrote nothing = %v", insp)
	}
	if insp["remote"] == "" || insp["last_event_id"] != "42" {
		t.Errorf("the request is not reported: %v", insp)
	}
	if insp["compression"] != "false" {
		t.Errorf("an uncompressed stream came out as %q", insp["compression"])
	}

	if err := c.Init(mock.NewMeta()); err != nil {
		t.Fatalf("init: %s", err)
	}
	if err := c.HandleMessage(gen.PID{}, Message{Event: "e", Data: []byte("payload")}); err != nil {
		t.Fatalf("write: %s", err)
	}

	insp = c.HandleInspect(gen.PID{})
	if insp["messages"] != "1" || insp["bytes_out"] == "0" || insp["last_message"] == "never" {
		t.Errorf("the written message was not counted: %v", insp)
	}
	if insp["write_failed"] != "0" || insp["uptime"] == "never started" {
		t.Errorf("inspect after a write = %v", insp)
	}

	queried := c.HandleInspect(gen.PID{}, "help", "nonsense")
	if queried["help"] == "" || queried["nonsense"] != "<unknown item>" {
		t.Errorf("queries came out as %v", queried)
	}
}

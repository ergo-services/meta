package tests

import (
	"bufio"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/meta"
	"ergo.services/ergo/testing/check"
	"ergo.services/ergo/testing/stage"

	"ergo.services/meta/sse"
)

// sseHost spawns an SSE handler and a web server on an ephemeral port. Connections
// are routed round-robin to the registered worker names passed at spawn.
type sseHost struct {
	act.Actor
	server gen.Alias
}

func factorySSEHost() gen.ProcessBehavior { return &sseHost{} }

func (h *sseHost) Init(args ...any) error {
	pool := args[0].([]gen.Atom)
	compression := false
	if len(args) > 1 {
		compression = args[1].(bool)
	}

	mux := http.NewServeMux()
	handler := sse.CreateHandler(sse.HandlerOptions{ProcessPool: pool, Heartbeat: time.Second, Compression: compression})
	if _, err := h.SpawnMeta(handler, gen.MetaOptions{}); err != nil {
		return err
	}
	mux.Handle("/sse", handler)

	ws, err := meta.CreateWebServer(meta.WebServerOptions{Host: "localhost", Port: 0, Handler: mux})
	if err != nil {
		return err
	}
	alias, err := h.SpawnMeta(ws, gen.MetaOptions{})
	if err != nil {
		return err
	}
	h.server = alias
	return nil
}

func (h *sseHost) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	if request == "addr" {
		insp, err := h.InspectMeta(h.server)
		if err != nil {
			return err, nil
		}
		return insp["listener"], nil
	}
	return "ok", nil
}

// sseWorker is a connection endpoint: it answers each new connection with a welcome
// event so the wire path (including the client's own read loop) is exercised.
type sseWorker struct{ act.Actor }

func factorySSEWorker() gen.ProcessBehavior { return &sseWorker{} }

func (w *sseWorker) HandleMessage(from gen.PID, message any) error {
	if m, ok := message.(sse.MessageConnect); ok {
		return w.SendAlias(m.ID, sse.Message{Event: "welcome", Data: []byte("hello"), MsgID: "0"})
	}
	return nil
}

// sseClientHost spawns an SSE client connection meta pointed at a server URL,
// directing its messages to the named sink process.
type sseClientHost struct{ act.Actor }

func factorySSEClientHost() gen.ProcessBehavior { return &sseClientHost{} }

func (h *sseClientHost) Init(args ...any) error {
	u, err := url.Parse(args[0].(string))
	if err != nil {
		return err
	}
	sink := args[1].(gen.Atom)
	client := sse.CreateConnection(sse.ConnectionOptions{URL: *u, Process: sink})
	_, err = h.SpawnMeta(client, gen.MetaOptions{})
	return err
}

// sink is a bare named receiver; the recorder observes messages routed to it.
type sink struct{ act.Actor }

func factorySink() gen.ProcessBehavior { return &sink{} }

func (s *sink) HandleMessage(from gen.PID, message any) error { return nil }

func isConnect(r check.Delivered) bool    { _, ok := r.Message.(sse.MessageConnect); return ok }
func isDisconnect(r check.Delivered) bool { _, ok := r.Message.(sse.MessageDisconnect); return ok }

func isWelcome(r check.Delivered) bool {
	m, ok := r.Message.(sse.Message)
	return ok && m.Event == "welcome" && string(m.Data) == "hello"
}

// name builds a ProcessID matching how a meta addresses a local registered name:
// the routed target carries an empty node.
func name(n gen.Atom) gen.ProcessID { return gen.ProcessID{Name: n} }

// sseURL calls the host for its bound web-server address and returns the /sse URL.
func sseURL(t *testing.T, n *stage.Node, host gen.PID) string {
	t.Helper()
	addr, err := n.Call(host, "addr")
	check.NoError(t, err)
	s, ok := addr.(string)
	check.True(t, ok)
	return "http://" + s + "/sse"
}

// openSSE starts a streaming SSE GET; the caller closes the body to disconnect.
func openSSE(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	check.NoError(t, err)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	check.NoError(t, err)
	return resp
}

// readEvent reads SSE lines until one event with data is assembled.
func readEvent(t *testing.T, resp *http.Response) (event, data string) {
	t.Helper()
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		check.NoError(t, err)
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if data != "" {
				return event, data
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

func TestSSE(t *testing.T) {
	// Connect: a new connection is routed to a worker, which answers with a welcome
	// event the client reads over the wire; closing the client reports a disconnect.
	t.Run("Connect", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("sse")
		n.SpawnRegister("worker", factorySSEWorker, gen.ProcessOptions{})
		host := n.Spawn(factorySSEHost, gen.ProcessOptions{}, []gen.Atom{"worker"})
		url := sseURL(t, n, host)

		mk := n.Mark()
		resp := openSSE(t, url, nil)
		n.ShouldDeliver().ToProcessID(name("worker")).Where(isConnect).Since(mk).Once().Within(2 * time.Second).Must()

		event, data := readEvent(t, resp)
		check.Equal(t, "welcome", event)
		check.Equal(t, "hello", data)

		mk2 := n.Mark()
		resp.Body.Close()
		n.ShouldDeliver().ToProcessID(name("worker")).Where(isDisconnect).Since(mk2).Once().Within(2 * time.Second).Must()
	})

	// Pool: successive connections are routed round-robin across the worker names.
	t.Run("Pool", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("sse")
		n.SpawnRegister("worker1", factorySSEWorker, gen.ProcessOptions{})
		n.SpawnRegister("worker2", factorySSEWorker, gen.ProcessOptions{})
		host := n.Spawn(factorySSEHost, gen.ProcessOptions{}, []gen.Atom{"worker1", "worker2"})
		url := sseURL(t, n, host)

		mk := n.Mark()
		r1 := openSSE(t, url, nil)
		defer r1.Body.Close()
		n.ShouldDeliver().ToProcessID(name("worker1")).Where(isConnect).Since(mk).Once().Within(2 * time.Second).Must()

		r2 := openSSE(t, url, nil)
		defer r2.Body.Close()
		n.ShouldDeliver().ToProcessID(name("worker2")).Where(isConnect).Since(mk).Once().Within(2 * time.Second).Must()
	})

	// LastEventID: a reconnecting client's Last-Event-ID header reaches the worker.
	t.Run("LastEventID", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("sse")
		n.SpawnRegister("worker", factorySSEWorker, gen.ProcessOptions{})
		host := n.Spawn(factorySSEHost, gen.ProcessOptions{}, []gen.Atom{"worker"})
		url := sseURL(t, n, host)

		header := http.Header{}
		header.Set("Last-Event-ID", "42")
		mk := n.Mark()
		resp := openSSE(t, url, header)
		defer resp.Body.Close()
		n.ShouldDeliver().ToProcessID(name("worker")).Where(func(r check.Delivered) bool {
			m, ok := r.Message.(sse.MessageLastEventID)
			return ok && m.LastEventID == "42"
		}).Since(mk).Once().Within(2 * time.Second).Must()
	})

	// Compression: with a gzip-capable client the server negotiates gzip and streams
	// a compressed welcome event that the client transparently decodes.
	t.Run("Compression", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("sse")
		n.SpawnRegister("worker", factorySSEWorker, gen.ProcessOptions{})
		host := n.Spawn(factorySSEHost, gen.ProcessOptions{}, []gen.Atom{"worker"}, true)
		url := sseURL(t, n, host)

		mk := n.Mark()
		resp := openSSE(t, url, nil) // net/http adds Accept-Encoding: gzip and decodes transparently
		n.ShouldDeliver().ToProcessID(name("worker")).Where(isConnect).Since(mk).Once().Within(2 * time.Second).Must()

		check.True(t, resp.Uncompressed) // the server compressed; the transport decoded

		event, data := readEvent(t, resp)
		check.Equal(t, "welcome", event)
		check.Equal(t, "hello", data)

		mk2 := n.Mark()
		resp.Body.Close()
		n.ShouldDeliver().ToProcessID(name("worker")).Where(isDisconnect).Since(mk2).Once().Within(2 * time.Second).Must()
	})

	// Client: the SSE client connection meta connects to the server and relays the
	// connect and the welcome event to its sink process.
	t.Run("Client", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("sse")
		n.SpawnRegister("worker", factorySSEWorker, gen.ProcessOptions{})
		n.SpawnRegister("clientsink", factorySink, gen.ProcessOptions{})
		host := n.Spawn(factorySSEHost, gen.ProcessOptions{}, []gen.Atom{"worker"})
		url := sseURL(t, n, host)

		mk := n.Mark()
		n.Spawn(factorySSEClientHost, gen.ProcessOptions{}, url, gen.Atom("clientsink"))
		n.ShouldDeliver().ToProcessID(name("clientsink")).Where(isConnect).Since(mk).Once().Within(3 * time.Second).Must()
		n.ShouldDeliver().ToProcessID(name("clientsink")).Where(isWelcome).Since(mk).Once().Within(3 * time.Second).Must()
	})
}

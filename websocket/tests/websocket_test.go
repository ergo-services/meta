package tests

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/meta"
	"ergo.services/ergo/testing/check"
	"ergo.services/ergo/testing/stage"

	"ergo.services/meta/websocket"

	gorilla "github.com/gorilla/websocket"
)

// wsHost spawns a websocket handler and a web server on an ephemeral port.
// Connections are routed round-robin to the registered worker names.
type wsHost struct {
	act.Actor
	server gen.Alias
}

func factoryWSHost() gen.ProcessBehavior { return &wsHost{} }

func (h *wsHost) Init(args ...any) error {
	pool := args[0].([]gen.Atom)

	mux := http.NewServeMux()
	handler := websocket.CreateHandler(websocket.HandlerOptions{ProcessPool: pool})
	if _, err := h.SpawnMeta(handler, gen.MetaOptions{}); err != nil {
		return err
	}
	mux.Handle("/ws", handler)

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

func (h *wsHost) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	if request == "addr" {
		insp, err := h.InspectMeta(h.server)
		if err != nil {
			return err, nil
		}
		return insp["listener"], nil
	}
	return "ok", nil
}

// wsWorker is a connection endpoint that echoes each incoming message back to the
// connection it came from.
type wsWorker struct{ act.Actor }

func factoryWSWorker() gen.ProcessBehavior { return &wsWorker{} }

func (w *wsWorker) HandleMessage(from gen.PID, message any) error {
	if m, ok := message.(websocket.Message); ok {
		return w.SendAlias(m.ID, m)
	}
	return nil
}

// wsClientHost spawns a websocket client connection meta pointed at a server URL,
// directing its messages to the named sink process. It exposes the meta alias and
// can exit the meta on command.
type wsClientHost struct {
	act.Actor
	conn gen.Alias
}

func factoryWSClientHost() gen.ProcessBehavior { return &wsClientHost{} }

func (h *wsClientHost) Init(args ...any) error {
	u, err := url.Parse(args[0].(string))
	if err != nil {
		return err
	}
	sink := args[1].(gen.Atom)
	conn, err := websocket.CreateConnection(websocket.ConnectionOptions{URL: *u, Process: sink})
	if err != nil {
		return err
	}
	id, err := h.SpawnMeta(conn, gen.MetaOptions{})
	if err != nil {
		return err
	}
	h.conn = id
	return nil
}

func (h *wsClientHost) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	switch request {
	case "meta":
		return h.conn, nil
	case "exit":
		h.SendExitMeta(h.conn, errors.New("bye"))
		return "ok", nil
	}
	return "ok", nil
}

// sink is a bare named receiver; the recorder observes messages routed to it.
type sink struct{ act.Actor }

func factorySink() gen.ProcessBehavior { return &sink{} }

func (s *sink) HandleMessage(from gen.PID, message any) error { return nil }

// wsLinker links itself to the connection alias, the way an owner process that
// wants to be torn down together with its connection does.
type wsLinker struct{ act.Actor }

func factoryWSLinker() gen.ProcessBehavior { return &wsLinker{} }

func (w *wsLinker) HandleMessage(from gen.PID, message any) error {
	if m, ok := message.(websocket.MessageConnect); ok {
		return w.LinkAlias(m.ID)
	}
	return nil
}

func (w *wsLinker) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	return "ok", nil
}

func isWSConnect(r check.Delivered) bool { _, ok := r.Message.(websocket.MessageConnect); return ok }
func isWSDisconnect(r check.Delivered) bool {
	_, ok := r.Message.(websocket.MessageDisconnect)
	return ok
}

func isWSBody(body string) func(check.Delivered) bool {
	return func(r check.Delivered) bool {
		m, ok := r.Message.(websocket.Message)
		return ok && string(m.Body) == body
	}
}

// name builds a ProcessID matching how a meta addresses a local registered name:
// the routed target carries an empty node.
func name(n gen.Atom) gen.ProcessID { return gen.ProcessID{Name: n} }

// wsURL calls the host for its bound web-server address and returns the /ws URL.
func wsURL(t *testing.T, n *stage.Node, host gen.PID) string {
	t.Helper()
	addr, err := n.Call(host, "addr")
	check.NoError(t, err)
	s, ok := addr.(string)
	check.True(t, ok)
	return "ws://" + s + "/ws"
}

// dial opens a websocket client connection with the gorilla dialer.
func dial(t *testing.T, url string) *gorilla.Conn {
	t.Helper()
	c, _, err := gorilla.DefaultDialer.Dial(url, nil)
	check.NoError(t, err)
	return c
}

func TestWebsocket(t *testing.T) {
	// Server: a connection is routed to a worker; a client message reaches the
	// worker, which echoes it back over the wire; closing reports a disconnect.
	t.Run("Server", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("ws")
		n.SpawnRegister("worker", factoryWSWorker, gen.ProcessOptions{})
		host := n.Spawn(factoryWSHost, gen.ProcessOptions{}, []gen.Atom{"worker"})
		url := wsURL(t, n, host)

		mk := n.Mark()
		c := dial(t, url)
		n.ShouldDeliver().ToProcessID(name("worker")).Where(isWSConnect).Since(mk).Once().Within(2 * time.Second).Must()

		check.NoError(t, c.WriteMessage(gorilla.TextMessage, []byte("hi")))
		n.ShouldDeliver().ToProcessID(name("worker")).Where(isWSBody("hi")).Since(mk).Once().Within(2 * time.Second).Must()

		_, echo, err := c.ReadMessage()
		check.NoError(t, err)
		check.Equal(t, "hi", string(echo))

		mk2 := n.Mark()
		c.Close()
		n.ShouldDeliver().ToProcessID(name("worker")).Where(isWSDisconnect).Since(mk2).Once().Within(2 * time.Second).Must()
	})

	// PeerGone: a socket that disappears without a close handshake is not a fault
	// of the meta process, so it terminates normally. A linked owner process then
	// gets a normal exit instead of being taken down abnormally with it.
	t.Run("PeerGone", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("ws")
		linker := n.SpawnRegister("linker", factoryWSLinker, gen.ProcessOptions{})
		host := n.Spawn(factoryWSHost, gen.ProcessOptions{}, []gen.Atom{"linker"})
		url := wsURL(t, n, host)

		mk := n.Mark()
		c := dial(t, url)
		n.ShouldDeliver().ToProcessID(name("linker")).Where(isWSConnect).Since(mk).Once().Within(2 * time.Second).Must()

		// barrier: the call is handled after LinkAlias returned
		_, err := n.Call(linker, "linked")
		check.NoError(t, err)

		mk2 := n.Mark()
		check.NoError(t, c.UnderlyingConn().Close())
		n.ShouldReceiveExit().To(linker).Reason(gen.TerminateReasonNormal).Since(mk2).Once().Within(2 * time.Second).Must()
		n.ShouldDeliver().ToProcessID(name("linker")).Where(isWSDisconnect).Since(mk2).Once().Within(2 * time.Second).Must()
	})

	// Pool: successive connections are routed round-robin across the worker names.
	t.Run("Pool", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("ws")
		n.SpawnRegister("worker1", factoryWSWorker, gen.ProcessOptions{})
		n.SpawnRegister("worker2", factoryWSWorker, gen.ProcessOptions{})
		host := n.Spawn(factoryWSHost, gen.ProcessOptions{}, []gen.Atom{"worker1", "worker2"})
		url := wsURL(t, n, host)

		mk := n.Mark()
		c1 := dial(t, url)
		defer c1.Close()
		n.ShouldDeliver().ToProcessID(name("worker1")).Where(isWSConnect).Since(mk).Once().Within(2 * time.Second).Must()

		c2 := dial(t, url)
		defer c2.Close()
		n.ShouldDeliver().ToProcessID(name("worker2")).Where(isWSConnect).Since(mk).Once().Within(2 * time.Second).Must()
	})

	// Client: the websocket client connection meta connects to the server, relays a
	// message written through it, receives the server's echo, and reports a
	// disconnect when the meta is exited.
	t.Run("Client", func(t *testing.T) {
		s := stage.New(t)
		n := s.StartNode("ws")
		n.SpawnRegister("worker", factoryWSWorker, gen.ProcessOptions{})
		n.SpawnRegister("clientsink", factorySink, gen.ProcessOptions{})
		host := n.Spawn(factoryWSHost, gen.ProcessOptions{}, []gen.Atom{"worker"})
		url := wsURL(t, n, host)

		mk := n.Mark()
		client := n.Spawn(factoryWSClientHost, gen.ProcessOptions{}, url, gen.Atom("clientsink"))
		n.ShouldDeliver().ToProcessID(name("clientsink")).Where(isWSConnect).Since(mk).Once().Within(3 * time.Second).Must()

		// write a message through the client meta; the worker echoes it back to the client
		metaAny, err := n.Call(client, "meta")
		check.NoError(t, err)
		metaID := metaAny.(gen.Alias)

		insp, err := n.Native().InspectMeta(metaID)
		check.NoError(t, err)
		check.Equal(t, "'clientsink'", insp["process"])

		mk2 := n.Mark()
		n.Send(metaID, websocket.Message{Type: websocket.MessageTypeText, Body: []byte("hi")})
		n.ShouldDeliver().ToProcessID(name("worker")).Where(isWSBody("hi")).Since(mk2).Once().Within(2 * time.Second).Must()
		n.ShouldDeliver().ToProcessID(name("clientsink")).Where(isWSBody("hi")).Since(mk2).Once().Within(2 * time.Second).Must()

		mk3 := n.Mark()
		_, err = n.Call(client, "exit")
		check.NoError(t, err)
		n.ShouldDeliver().ToProcessID(name("clientsink")).Where(isWSDisconnect).Since(mk3).Once().Within(2 * time.Second).Must()
	})
}

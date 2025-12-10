package tests

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/meta"
	"ergo.services/ergo/node"

	"ergo.services/meta/sse"
)

var (
	t1cases []*testcase
)

func factory_t1() gen.ProcessBehavior {
	return &t1{}
}

type t1 struct {
	act.Actor

	testcase *testcase
}

func factory_t1web() gen.ProcessBehavior {
	return &t1web{}
}

type t1web struct {
	act.Actor

	tc *testcase
}

func (t *t1web) Init(args ...any) error {
	t.tc = args[0].(*testcase)

	mux := http.NewServeMux()

	// SSE handler with pool
	sseopts := sse.HandlerOptions{
		ProcessPool: []gen.Atom{"pool_handler1", "pool_handler2"},
		Heartbeat:   5 * time.Second,
	}
	pool_handler := sse.CreateHandler(sseopts)
	if _, err := t.SpawnMeta(pool_handler, gen.MetaOptions{}); err != nil {
		t.Log().Error("unable to spawn meta process (pool handler)")
		return err
	}
	mux.Handle("/pool", pool_handler)

	// SSE handler without pool (handled by web process itself)
	sseopts2 := sse.HandlerOptions{
		Heartbeat: 5 * time.Second,
	}
	handler := sse.CreateHandler(sseopts2)
	if _, err := t.SpawnMeta(handler, gen.MetaOptions{}); err != nil {
		t.Log().Error("unable to spawn meta process (handler)")
		return err
	}
	mux.Handle("/sse", handler)

	// create and spawn web server meta process
	serverOptions := meta.WebServerOptions{
		Port:    12122,
		Host:    "localhost",
		Handler: mux,
	}

	webserver, err := meta.CreateWebServer(serverOptions)
	if err != nil {
		return err
	}
	if _, err := t.SpawnMeta(webserver, gen.MetaOptions{}); err != nil {
		return err
	}
	return nil
}

func factory_t1handler() gen.ProcessBehavior {
	return &t1handler{}
}

type t1handler struct {
	act.Actor
	tc          *testcase
	connections map[gen.Alias]bool
}

func (t *t1handler) Init(args ...any) error {
	t.tc = args[0].(*testcase)
	t.connections = make(map[gen.Alias]bool)
	return nil
}

func (t *t1handler) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case sse.MessageConnect:
		t.Log().Info("handler process - new SSE connection: %v", m.ID)
		t.connections[m.ID] = true
		t.tc.output = message
		t.tc.err <- nil

	case sse.MessageLastEventID:
		t.Log().Info("handler process - client reconnected with Last-Event-ID: %s", m.LastEventID)
		t.tc.output = message
		t.tc.err <- nil

	case sse.MessageDisconnect:
		t.Log().Info("handler process - SSE disconnected: %v", m.ID)
		delete(t.connections, m.ID)
		t.tc.output = message
		t.tc.err <- nil

	case string:
		// send SSE message to all connections
		t.Log().Info("handler process - sending message to %d connections", len(t.connections))
		for connID := range t.connections {
			msg := sse.Message{
				Event: "test",
				Data:  []byte(m),
				MsgID: "1",
			}
			if err := t.SendAlias(connID, msg); err != nil {
				t.Log().Error("unable to send SSE message: %s", err)
			}
		}

	default:
		t.Log().Info("handler process got unknown message from %s: %#v", from, message)
	}

	return nil
}

func (t *t1handler) Terminate(reason error) {
	t.Log().Info("terminated with reason: %s", reason)
}

func (t *t1web) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case sse.MessageConnect:
		t.Log().Info("web process - new SSE connection: %v", m.ID)
		t.tc.output = message
		t.tc.err <- nil

		// send a welcome message
		msg := sse.Message{
			Event: "welcome",
			Data:  []byte("Hello from server"),
			MsgID: "0",
		}
		if err := t.SendAlias(m.ID, msg); err != nil {
			t.Log().Error("unable to send welcome message: %s", err)
		}

	case sse.MessageDisconnect:
		t.Log().Info("web process - SSE disconnected: %v", m.ID)
		t.tc.output = message
		t.tc.err <- nil

	default:
		t.Log().Info("web process got unknown message from %s: %#v", from, message)
	}

	return nil
}

func (t *t1web) Terminate(reason error) {
	t.Log().Info("terminated with reason: %s", reason)
}

func (t *t1) HandleMessage(from gen.PID, message any) error {
	if t.testcase == nil {
		t.testcase = message.(*testcase)
		message = initcase{}
	}
	method := reflect.ValueOf(t).MethodByName(t.testcase.name)
	if method.IsValid() == false {
		t.testcase.err <- fmt.Errorf("unknown method %q", t.testcase.name)
		t.testcase = nil
		return nil
	}
	method.Call([]reflect.Value{reflect.ValueOf(message)})
	return nil
}

func (t *t1) TestServer(input any) {
	defer func() {
		t.testcase = nil
	}()

	webtc := &testcase{"", nil, nil, make(chan error)}
	webpid, err := t.Spawn(factory_t1web, gen.ProcessOptions{}, webtc)
	if err != nil {
		t.Log().Error("unable to spawn web process: %s", err)
		t.testcase.err <- err
		return
	}

	pool_handler1_tc := &testcase{"", nil, nil, make(chan error)}
	pool_handler1, err := t.SpawnRegister("pool_handler1", factory_t1handler, gen.ProcessOptions{}, pool_handler1_tc)
	if err != nil {
		t.Log().Error("unable to spawn pool_handler1 process: %s", err)
		t.testcase.err <- err
		return
	}

	pool_handler2_tc := &testcase{"", nil, nil, make(chan error)}
	pool_handler2, err := t.SpawnRegister("pool_handler2", factory_t1handler, gen.ProcessOptions{}, pool_handler2_tc)
	if err != nil {
		t.Log().Error("unable to spawn pool_handler2 process: %s", err)
		t.testcase.err <- err
		return
	}

	t.Log().Info("web: %s, pool handlers: %s %s", webpid, pool_handler1, pool_handler2)

	time.Sleep(100 * time.Millisecond)

	// test 1: Direct SSE handler (handled by web process)
	t.Log().Info("Test 1: Direct SSE handler")
	{
		resp, err := http.Get("http://localhost:12122/sse")
		if err != nil {
			t.Log().Error("unable to connect to SSE endpoint: %s", err)
			t.testcase.err <- err
			return
		}

		// wait for MessageConnect
		if err := webtc.wait(2); err != nil {
			t.Log().Error("web process didn't receive sse.MessageConnect: %s", err)
			t.testcase.err <- err
			resp.Body.Close()
			return
		}
		if _, ok := webtc.output.(sse.MessageConnect); ok == false {
			t.Log().Error("incorrect message %#v (exp MessageConnect)", webtc.output)
			t.testcase.err <- errIncorrect
			resp.Body.Close()
			return
		}

		// read SSE response
		reader := bufio.NewReader(resp.Body)

		// read and parse SSE events
		var event, data string
		eventReceived := false
		for eventReceived == false {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Log().Error("error reading SSE: %s", err)
				break
			}
			t.Log().Info("received line: %q", line)
			line = strings.TrimRight(line, "\r\n")

			// empty line means end of event
			if line == "" {
				if data != "" {
					eventReceived = true
				}
				continue
			}

			// skip comments
			if strings.HasPrefix(line, ":") {
				continue
			}

			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}

		if event != "welcome" || data != "Hello from server" {
			t.Log().Error("incorrect SSE event: event=%q data=%q", event, data)
			t.testcase.err <- errIncorrect
			resp.Body.Close()
			return
		}
		t.Log().Info("received welcome event correctly")

		resp.Body.Close()

		// wait for MessageDisconnect
		if err := webtc.wait(2); err != nil {
			t.Log().Error("web process didn't receive sse.MessageDisconnect: %s", err)
			t.testcase.err <- err
			return
		}
	}

	// test 2: Pool handler (round-robin)
	t.Log().Info("Test 2: Pool handler - first connection")
	{
		resp, err := http.Get("http://localhost:12122/pool")
		if err != nil {
			t.Log().Error("unable to connect to pool SSE endpoint: %s", err)
			t.testcase.err <- err
			return
		}

		// should be handled by pool_handler1
		if err := pool_handler1_tc.wait(2); err != nil {
			t.Log().Error("pool_handler1 didn't receive sse.MessageConnect: %s", err)
			t.testcase.err <- err
			resp.Body.Close()
			return
		}
		if _, ok := pool_handler1_tc.output.(sse.MessageConnect); ok == false {
			t.Log().Error("incorrect message %#v (exp MessageConnect)", pool_handler1_tc.output)
			t.testcase.err <- errIncorrect
			resp.Body.Close()
			return
		}

		resp.Body.Close()

		// wait for disconnect
		if err := pool_handler1_tc.wait(2); err != nil {
			t.Log().Error("pool_handler1 didn't receive sse.MessageDisconnect: %s", err)
			t.testcase.err <- err
			return
		}
	}

	// test 3: Second pool connection (should go to pool_handler2)
	t.Log().Info("Test 3: Pool handler - second connection")
	{
		resp, err := http.Get("http://localhost:12122/pool")
		if err != nil {
			t.Log().Error("unable to connect to pool SSE endpoint: %s", err)
			t.testcase.err <- err
			return
		}

		// should be handled by pool_handler2
		if err := pool_handler2_tc.wait(2); err != nil {
			t.Log().Error("pool_handler2 didn't receive sse.MessageConnect: %s", err)
			t.testcase.err <- err
			resp.Body.Close()
			return
		}
		if _, ok := pool_handler2_tc.output.(sse.MessageConnect); ok == false {
			t.Log().Error("incorrect message %#v (exp MessageConnect)", pool_handler2_tc.output)
			t.testcase.err <- errIncorrect
			resp.Body.Close()
			return
		}

		resp.Body.Close()

		// wait for disconnect
		if err := pool_handler2_tc.wait(2); err != nil {
			t.Log().Error("pool_handler2 didn't receive sse.MessageDisconnect: %s", err)
			t.testcase.err <- err
			return
		}
	}

	t.Node().Kill(webpid)
	t.testcase.err <- nil
}

func factory_t1client() gen.ProcessBehavior {
	return &t1client{}
}

type t1client struct {
	act.Actor
	tc     *testcase
	metaID gen.Alias
}

func (t *t1client) Init(args ...any) error {
	t.tc = args[0].(*testcase)

	opt := sse.ConnectionOptions{
		URL: url.URL{Scheme: "http", Host: "localhost:12122", Path: "/sse"},
	}
	ssec := sse.CreateConnection(opt)
	id, err := t.SpawnMeta(ssec, gen.MetaOptions{})
	if err != nil {
		t.Log().Error("unable to spawn meta process: %s", err)
		return err
	}

	t.metaID = id
	t.tc.input = id

	return nil
}

func (t *t1client) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case sse.MessageConnect:
		t.Log().Info("client process - connected: %v", m.ID)
		t.tc.output = m
		t.tc.err <- nil

	case sse.MessageDisconnect:
		t.Log().Info("client process - disconnected: %v", m.ID)
		t.tc.output = m
		t.tc.err <- nil

	case sse.Message:
		t.Log().Info("client process - got SSE event: %s data=%s", m.Event, string(m.Data))
		t.tc.output = m
		t.tc.err <- nil

	default:
		t.Log().Info("client process got unknown message from %s: %#v", from, message)
	}

	return nil
}

func (t *t1) TestClient(input any) {
	defer func() {
		t.testcase = nil
	}()

	webtc := &testcase{"", nil, nil, make(chan error)}
	webpid, err := t.Spawn(factory_t1web, gen.ProcessOptions{}, webtc)
	if err != nil {
		t.Log().Error("unable to spawn web process: %s", err)
		t.testcase.err <- err
		return
	}
	t.Log().Info("started web process: %s", webpid)

	time.Sleep(100 * time.Millisecond)

	clienttc := &testcase{"", nil, nil, make(chan error)}
	clientpid, err := t.Spawn(factory_t1client, gen.ProcessOptions{}, clienttc)
	if err != nil {
		t.Log().Error("unable to spawn client process: %s", err)
		t.testcase.err <- err
		return
	}
	t.Log().Info("started client process: %s", clientpid)

	// client should receive MessageConnect
	if err := clienttc.wait(2); err != nil {
		t.Log().Error("client process didn't receive sse.MessageConnect: %s", err)
		t.testcase.err <- err
		return
	}
	if _, ok := clienttc.output.(sse.MessageConnect); ok == false {
		t.Log().Error("client process received wrong message (exp: sse.MessageConnect) %#v", clienttc.output)
		t.testcase.err <- errIncorrect
		return
	}

	// web process should receive MessageConnect from server handler
	if err := webtc.wait(2); err != nil {
		t.Log().Error("web process didn't receive sse.MessageConnect: %s", err)
		t.testcase.err <- err
		return
	}

	// client should receive the welcome message
	if err := clienttc.wait(2); err != nil {
		t.Log().Error("client process didn't receive SSE message: %s", err)
		t.testcase.err <- err
		return
	}
	msg, ok := clienttc.output.(sse.Message)
	if ok == false {
		t.Log().Error("client process received wrong message (exp: sse.Message) %#v", clienttc.output)
		t.testcase.err <- errIncorrect
		return
	}
	if msg.Event != "welcome" || string(msg.Data) != "Hello from server" {
		t.Log().Error("incorrect SSE message: event=%q data=%q", msg.Event, string(msg.Data))
		t.testcase.err <- errIncorrect
		return
	}

	// kill the client
	t.Node().Kill(clientpid)

	time.Sleep(100 * time.Millisecond)

	t.Node().Kill(webpid)
	t.testcase.err <- nil
}

func TestT1(t *testing.T) {
	nopt := gen.NodeOptions{}
	nopt.Log.DefaultLogger.Disable = true
	//nopt.Log.Level = gen.LogLevelTrace
	n, err := node.Start("t1SSEnode@localhost", nopt, gen.Version{})
	if err != nil {
		t.Fatal(err)
	}

	popt := gen.ProcessOptions{}
	pid, err := n.Spawn(factory_t1, popt)
	if err != nil {
		panic(err)
	}

	t1cases = []*testcase{
		{"TestServer", nil, nil, make(chan error)},
		{"TestClient", nil, nil, make(chan error)},
	}
	for _, tc := range t1cases {
		name := tc.name
		if tc.input != nil {
			name = fmt.Sprintf("%s:%s", tc.name, tc.input)
		}
		t.Run(name, func(t *testing.T) {
			n.Send(pid, tc)
			if err := tc.wait(30); err != nil {
				t.Fatal(err)
			}
		})
	}

	n.StopForce()
}

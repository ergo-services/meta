package tests

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/meta"
	"ergo.services/ergo/node"

	"ergo.services/meta/websocket"

	gorilla "github.com/gorilla/websocket"
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

	wsopts := websocket.HandlerOptions{
		ProcessPool: []gen.Atom{"pool_handler1", "pool_handler2"},
	}
	pool_handler := websocket.CreateHandler(wsopts)
	if _, err := t.SpawnMeta(pool_handler, gen.MetaOptions{}); err != nil {
		t.Log().Error("unable to spawn meta process (pool handler)")
		return err
	}
	mux.Handle("/pool", pool_handler)

	wsopts2 := websocket.HandlerOptions{}
	handler := websocket.CreateHandler(wsopts2)
	if _, err := t.SpawnMeta(handler, gen.MetaOptions{}); err != nil {
		t.Log().Error("unable to spawn meta process (handler)")
		return err
	}
	mux.Handle("/ws", handler)

	// create and spawn web server meta process
	serverOptions := meta.WebServerOptions{
		Port:    12121,
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
	tc *testcase
}

func (t *t1handler) Init(args ...any) error {
	t.tc = args[0].(*testcase)
	return nil
}

func (t *t1handler) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case websocket.MessageConnect:
		t.Log().Info("handler process - new connection: %v", m)
		t.tc.output = message
		t.tc.err <- nil
	case websocket.MessageDisconnect:
		t.Log().Info("handler process - disconnected: %v", m)
		t.tc.output = message
		t.tc.err <- nil
	case websocket.Message:
		t.Log().Info("handler process - got message: %v", m)
		t.tc.output = message
		t.tc.err <- nil
		// just echo reply
		if err := t.SendAlias(m.ID, m); err != nil {
			t.Log().Error("unable to send: %s", err)
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
	case websocket.MessageConnect:
		t.Log().Info("web process - new connection: %v", m)
		t.tc.output = message
		t.tc.err <- nil
	case websocket.MessageDisconnect:
		t.Log().Info("web process - disconnected: %v", m)
		t.tc.output = message
		t.tc.err <- nil
	case websocket.Message:
		t.Log().Info("web process - got message from %s: %q", m.ID, string(m.Body))
		t.tc.output = message
		t.tc.err <- nil
		// just echo reply
		if err := t.SendAlias(m.ID, m); err != nil {
			t.Log().Error("unable to send: %s", err)
			break
		}
		t.Log().Info("web process - sent echo reply")
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
	// get method by name
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
	// start web-process
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

	// test handling websocket messages by the handler-process itself

	for i := 0; i < 4; i++ {
		var tc *testcase
		var url string
		switch i {
		case 0:
			// web-server process must handle by itself
			url = "ws://localhost:12121/ws"
			tc = webtc
		case 1:
			// must be handled by pool_handler1
			url = "ws://localhost:12121/pool"
			tc = pool_handler1_tc
		case 2:
			// must be handled by pool_handler2
			url = "ws://localhost:12121/pool"
			tc = pool_handler2_tc
		case 3:
			// must be handled by pool_handler1
			url = "ws://localhost:12121/pool"
			tc = pool_handler1_tc
		default:
			panic("no case")
		}

		t.Log().Info("case %d", i)

		c, _, err := gorilla.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Log().Error("unable to connect to %q: %s", url, err)
			t.testcase.err <- err
			return
		}
		if err := tc.wait(1); err != nil {
			t.Log().Error("handle process hasnt received websocket.MessageConnect message: %s", err)
			t.testcase.err <- err
			return
		}
		if _, ok := tc.output.(websocket.MessageConnect); ok == false {
			t.Log().Error("incorrect message %#v (exp MessageConnect)", tc.output)
			t.testcase.err <- errIncorrect
			return
		}

		msg := []byte("hi")
		if err := c.WriteMessage(gorilla.TextMessage, msg); err != nil {
			t.Log().Error("unable to write to the websocket: %s", err)
			t.testcase.err <- err
			return
		}

		if err := tc.wait(1); err != nil {
			t.Log().Error("web process hasnt received websocket.Message message: %s", err)
			t.testcase.err <- err
			return
		}

		if _, ok := tc.output.(websocket.Message); ok == false {
			t.Log().Error("incorrect message %#v (exp Message)", tc.output)
			t.testcase.err <- errIncorrect
			return
		}

		if err != nil {
			t.Log().Error("unable to read data from websocket: %s", err)
			t.testcase.err <- err
			return
		}
		_, recv, err := c.ReadMessage()
		if reflect.DeepEqual(msg, recv) == false {
			t.Log().Error("incorrect data: %#v (exp: %#v)", recv, msg)
			t.testcase.err <- errIncorrect
			return
		}

		c.Close()
		if err := tc.wait(1); err != nil {
			t.Log().Error("web process hasnt received websocket.MessageDisconnect message: %s", err)
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
	tc *testcase
}

func (t *t1client) Init(args ...any) error {
	t.tc = args[0].(*testcase)

	opt := websocket.ConnectionOptions{
		URL: url.URL{Scheme: "ws", Host: "localhost:12121", Path: "/ws"},
	}
	wsc, err := websocket.CreateConnection(opt)
	if err != nil {
		t.Log().Error("unable to create websocket connection: %s", err)
		return err
	}
	id, err := t.SpawnMeta(wsc, gen.MetaOptions{})
	if err != nil {
		t.Log().Error("unable to spawn meta process: %s", err)
		wsc.Terminate(err)
		return err
	}

	t.tc.input = id

	return nil
}

func (t *t1client) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case websocket.MessageConnect:
		t.Log().Info("client process - new connection: %v", m)
		t.tc.output = m
		t.tc.err <- nil
		t.Log().Info("client process - new connection out")
	case websocket.MessageDisconnect:
		t.Log().Info("client process - disconnected: %v", m)
		t.tc.output = m
		t.tc.err <- nil
		t.Log().Info("client process - disconnected out")
	case websocket.Message:
		t.tc.output = m
		t.Log().Info("client process - got message: %v", m)
		t.tc.err <- nil
		t.Log().Info("client process - got message out")
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
	// start web-process
	webpid, err := t.Spawn(factory_t1web, gen.ProcessOptions{}, webtc)
	if err != nil {
		t.Log().Error("unable to spawn web process: %s", err)
		t.testcase.err <- err
		return
	}
	t.Log().Info("started web process: %s", webpid)

	clienttc := &testcase{"", nil, nil, make(chan error)}
	// start handler-process
	clientpid, err := t.Spawn(factory_t1client, gen.ProcessOptions{}, clienttc)
	if err != nil {
		t.Log().Error("unable to spawn client process: %s", err)
		t.testcase.err <- err
		return
	}
	t.Log().Info("started client process: %s", clientpid)
	metaid := clienttc.input.(gen.Alias)

	if err := clienttc.wait(1); err != nil {
		t.Log().Error("client process seemed didn't receive websocket.MessageConnect message")
		t.testcase.err <- err
		return
	}
	if _, ok := clienttc.output.(websocket.MessageConnect); ok == false {
		t.Log().Error("client process seemed received wrong message (exp: websocket.MessageConnect) %#v", clienttc.output)
		t.testcase.err <- err
		return
	}

	// web-process should receive websocket.MessageConnect
	if err := webtc.wait(1); err != nil {
		t.Log().Error("web process seemed didn't receive websocket.MessageConnect message")
		t.testcase.err <- err
		return
	}

	// ask meta process that serves client connection to send a websocket message
	msg := websocket.Message{
		Type: gorilla.TextMessage,
		Body: []byte("hi"),
	}
	t.Send(metaid, msg)

	// web-process should receive websocket.Message
	if err := webtc.wait(1); err != nil {
		t.Log().Error("web process seemed didn't receive websocket.Message message")
		t.testcase.err <- err
		return
	}

	// web-process received message and should send an echo-message.
	// client process should receive websocket.Message

	if err := clienttc.wait(1); err != nil {
		t.Log().Error("client process seemed didn't receive echo websocket.Message")
		t.testcase.err <- err
	}

	// send exit message to the meta process that serves client connection
	t.SendExitMeta(metaid, errors.New("ttt"))
	// client process should receive websocket.MessageDisconnect
	if err := clienttc.wait(1); err != nil {
		t.Log().Error("client process seemed didn't receive websocket.MessageDisconnect")
		t.testcase.err <- err
	}
	if err := webtc.wait(1); err != nil {
		t.Log().Error("webp rocess seemed didn't receive websocket.MessageDisconnect")
		t.testcase.err <- err
	}

	t.testcase.err <- nil
}

func TestT1(t *testing.T) {
	nopt := gen.NodeOptions{}
	nopt.Log.DefaultLogger.Disable = true
	//nopt.Log.Level = gen.LogLevelTrace
	node, err := node.Start("t1WebSocketnode@localhost", nopt, gen.Version{})
	if err != nil {
		t.Fatal(err)
	}

	popt := gen.ProcessOptions{}
	pid, err := node.Spawn(factory_t1, popt)
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
			node.Send(pid, tc)
			if err := tc.wait(30); err != nil {
				t.Fatal(err)
			}
		})
	}

	node.StopForce()
}

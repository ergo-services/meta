package websocket

import (
	"net/http"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
	"ergo.services/ergo/meta"
	ws "github.com/gorilla/websocket"
)

func CreateHandler(options HandlerOptions) meta.WebHandler {
	if options.HandshakeTimeout == 0 {
		options.HandshakeTimeout = 15 * time.Second
	}
	w := &wsh{
		pool: options.ProcessPool,
		upgrader: ws.Upgrader{
			HandshakeTimeout:  options.HandshakeTimeout,
			EnableCompression: options.EnableCompression,
			CheckOrigin:       options.CheckOrigin,
		},
		ch: make(chan error),
	}
	return w
}

type HandlerOptions struct {
	ProcessPool       []gen.Atom
	HandshakeTimeout  time.Duration
	EnableCompression bool
	CheckOrigin       func(r *http.Request) bool
}

type wsh struct {
	gen.MetaProcess

	pool       []gen.Atom
	i          int32
	upgrader   ws.Upgrader
	terminated bool
	ch         chan error
}

// Handler gen.MetaBehavior implementation
func (w *wsh) Init(process gen.MetaProcess) error {
	w.MetaProcess = process
	w.i = -1
	return nil
}

func (w *wsh) Start() error {
	return <-w.ch
}

func (w *wsh) HandleMessage(from gen.PID, message any) error {
	return nil
}

func (w *wsh) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	return nil, nil
}

func (w *wsh) Terminate(reason error) {
	w.terminated = true
	w.ch <- reason
	close(w.ch)
}

func (w *wsh) HandleInspect(from gen.PID) map[string]string {
	return nil
}

//
// Handler http.Handler implementation
//

func (w *wsh) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if w.MetaProcess == nil {
		http.Error(writer, "Handler is not initialized", http.StatusServiceUnavailable)
		return
	}

	if w.terminated {
		http.Error(writer, "Handler terminated", http.StatusServiceUnavailable)
		return
	}

	conn, err := w.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		w.Log().Error("can not upgrade to websocket: %s", err)
		return
	}
	c := &wsc{conn: conn}
	if l := len(w.pool); l > 0 {
		i := int(atomic.AddInt32(&w.i, 1))
		c.process = w.pool[i%l]
	}

	if _, err := w.Spawn(c, gen.MetaOptions{}); err != nil {
		conn.Close()
		w.Log().Error("unable to spawn meta process: %s", err)
	}
}

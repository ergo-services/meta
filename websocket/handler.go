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
	return &handler{
		pool: options.ProcessPool,
		upgrader: ws.Upgrader{
			HandshakeTimeout:  options.HandshakeTimeout,
			EnableCompression: options.EnableCompression,
			CheckOrigin:       options.CheckOrigin,
		},
		ch: make(chan error),
	}
}

type HandlerOptions struct {
	ProcessPool       []gen.Atom
	HandshakeTimeout  time.Duration
	EnableCompression bool
	CheckOrigin       func(r *http.Request) bool
}

type handler struct {
	gen.MetaProcess

	pool       []gen.Atom
	i          int32
	upgrader   ws.Upgrader
	terminated bool
	ch         chan error
}

// Handler gen.MetaBehavior implementation
func (h *handler) Init(process gen.MetaProcess) error {
	h.MetaProcess = process
	h.i = -1
	return nil
}

func (h *handler) Start() error {
	return <-h.ch
}

func (h *handler) HandleMessage(from gen.PID, message any) error {
	return nil
}

func (h *handler) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	return nil, nil
}

func (h *handler) Terminate(reason error) {
	h.terminated = true
	h.ch <- reason
	close(h.ch)
}

func (h *handler) HandleInspect(from gen.PID, item ...string) map[string]string {
	return nil
}

//
// Handler http.Handler implementation
//

func (h *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if h.MetaProcess == nil {
		http.Error(writer, "Handler is not initialized", http.StatusServiceUnavailable)
		return
	}

	if h.terminated {
		http.Error(writer, "Handler terminated", http.StatusServiceUnavailable)
		return
	}

	conn, err := h.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		h.Log().Error("can not upgrade to websocket: %s", err)
		return
	}
	c := &connection{conn: conn}
	if l := len(h.pool); l > 0 {
		i := int(atomic.AddInt32(&h.i, 1))
		c.process = h.pool[i%l]
	}

	if _, err := h.Spawn(c, gen.MetaOptions{}); err != nil {
		conn.Close()
		h.Log().Error("unable to spawn meta process: %s", err)
	}
}

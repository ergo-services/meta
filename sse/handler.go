package sse

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
	"ergo.services/ergo/meta"
)

const (
	defaultHeartbeat = 30 * time.Second
)

// CreateHandler creates a new SSE handler meta-process
func CreateHandler(options HandlerOptions) meta.WebHandler {
	if options.Heartbeat == 0 {
		options.Heartbeat = defaultHeartbeat
	}
	return &handler{
		pool:      options.ProcessPool,
		heartbeat: options.Heartbeat,
		ch:        make(chan error),
	}
}

// HandlerOptions defines options for the SSE handler
type HandlerOptions struct {
	ProcessPool []gen.Atom    // Worker processes for handling connections (round-robin)
	Heartbeat   time.Duration // Heartbeat interval for keeping connection alive
}

type handler struct {
	gen.MetaProcess

	pool       []gen.Atom
	i          int32
	heartbeat  time.Duration
	terminated bool
	ch         chan error
}

//
// gen.MetaBehavior implementation
//

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
// http.Handler implementation
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

	// check if client accepts SSE
	accept := request.Header.Get("Accept")
	if accept != "" && accept != "text/event-stream" && accept != "*/*" {
		http.Error(writer, "Not Acceptable", http.StatusNotAcceptable)
		return
	}

	flusher, ok := writer.(http.Flusher)
	if ok == false {
		http.Error(writer, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// set SSE headers
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	c := &serverConnection{
		writer:    writer,
		flusher:   flusher,
		request:   request,
		heartbeat: h.heartbeat,
		done:      make(chan struct{}),
	}

	if l := len(h.pool); l > 0 {
		i := int(atomic.AddInt32(&h.i, 1))
		c.process = h.pool[i%l]
	}

	if _, err := h.Spawn(c, gen.MetaOptions{}); err != nil {
		http.Error(writer, "Unable to handle connection", http.StatusInternalServerError)
		h.Log().Error("unable to spawn SSE connection meta process: %s", err)
		return
	}

	// block until connection is done
	<-c.done
}

//
// serverConnection - meta-process for each SSE connection
//

type serverConnection struct {
	gen.MetaProcess

	writer    http.ResponseWriter
	flusher   http.Flusher
	process   gen.Atom
	request   *http.Request
	heartbeat time.Duration
	done      chan struct{}
	bytesOut  uint64
}

func (c *serverConnection) Init(process gen.MetaProcess) error {
	c.MetaProcess = process
	return nil
}

func (c *serverConnection) Start() error {
	var to any

	id := c.ID()

	if c.process == "" {
		to = c.Parent()
	} else {
		to = c.process
	}

	defer func() {
		message := MessageDisconnect{
			ID: id,
		}
		if err := c.Send(to, message); err != nil {
			c.Log().Error("unable to send sse.MessageDisconnect: %s", err)
		}
		close(c.done)
	}()

	// send connect message
	message := MessageConnect{
		ID:      id,
		Request: c.request,
	}

	// get remote/local addresses from request
	if c.request.RemoteAddr != "" {
		message.RemoteAddr = &addr{network: "tcp", address: c.request.RemoteAddr}
	}
	if c.request.Context() != nil {
		if localAddr, ok := c.request.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			message.LocalAddr = localAddr
		}
	}

	if err := c.Send(to, message); err != nil {
		c.Log().Error("unable to send sse.MessageConnect to %v: %s", to, err)
		return err
	}

	// check for Last-Event-ID header (client reconnection)
	lastEventID := c.request.Header.Get("Last-Event-ID")
	if lastEventID != "" {
		msg := MessageLastEventID{
			ID:          id,
			LastEventID: lastEventID,
		}
		if err := c.Send(to, msg); err != nil {
			c.Log().Error("unable to send sse.MessageLastEventID: %s", err)
		}
	}

	// send initial heartbeat comment to establish connection
	if _, err := c.writer.Write([]byte(": connected\n\n")); err != nil {
		return err
	}
	c.flusher.Flush()

	// wait for client disconnect using request context
	// (context is cancelled when the client disconnects or the server handler returns)
	<-c.request.Context().Done()

	return nil
}

func (c *serverConnection) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case Message:
		data := formatSSE(m)
		if _, err := c.writer.Write(data); err != nil {
			c.Log().Error("unable to write SSE data: %s", err)
			return err
		}
		c.flusher.Flush()
		atomic.AddUint64(&c.bytesOut, uint64(len(data)))

	default:
		c.Log().Error("unsupported message from %s. ignored", from)
	}
	return nil
}

func (c *serverConnection) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	return gen.ErrUnsupported, nil
}

func (c *serverConnection) Terminate(reason error) {
	// c.done is closed in Start() defer, so we don't close it here

	if reason == nil || reason == gen.TerminateReasonNormal {
		return
	}
	c.Log().Error("terminated abnormally: %s", reason)
}

func (c *serverConnection) HandleInspect(from gen.PID, item ...string) map[string]string {
	bytesOut := atomic.LoadUint64(&c.bytesOut)
	result := map[string]string{
		"process":   c.process.String(),
		"bytes out": fmt.Sprintf("%d", bytesOut),
	}
	if c.request.RemoteAddr != "" {
		result["remote"] = c.request.RemoteAddr
	}
	return result
}

// formatSSE formats a Message as SSE wire format
func formatSSE(msg Message) []byte {
	var buf bytes.Buffer

	if msg.Event != "" {
		buf.WriteString("event: ")
		buf.WriteString(msg.Event)
		buf.WriteByte('\n')
	}

	if msg.MsgID != "" {
		buf.WriteString("id: ")
		buf.WriteString(msg.MsgID)
		buf.WriteByte('\n')
	}

	if msg.Retry > 0 {
		buf.WriteString("retry: ")
		buf.WriteString(strconv.Itoa(msg.Retry))
		buf.WriteByte('\n')
	}

	// handle multi-line data
	if len(msg.Data) > 0 {
		lines := bytes.Split(msg.Data, []byte("\n"))
		for _, line := range lines {
			buf.WriteString("data: ")
			buf.Write(line)
			buf.WriteByte('\n')
		}
	} else {
		buf.WriteString("data: \n")
	}

	buf.WriteByte('\n') // end of event
	return buf.Bytes()
}

// addr implements net.Addr for remote address string
type addr struct {
	network string
	address string
}

func (a *addr) Network() string {
	return a.network
}

func (a *addr) String() string {
	return a.address
}

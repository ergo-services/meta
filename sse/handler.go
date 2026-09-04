package sse

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
	"ergo.services/ergo/meta"
)

const (
	defaultHeartbeat = 30 * time.Second
)

// defaultBeat is what a quiet stream gets when nothing else is configured.
var defaultBeat = []byte(":\r\n")

// acceptsStream reports whether the client takes an event stream. Accept is a list.
func acceptsStream(accept string) bool {
	if accept == "" {
		return true
	}
	for _, entry := range strings.Split(accept, ",") {
		media, _, _ := strings.Cut(entry, ";")
		switch strings.ToLower(strings.TrimSpace(media)) {
		case "text/event-stream", "text/*", "*/*":
			return true
		}
	}
	return false
}

// CreateHandler creates a new SSE handler meta-process
func CreateHandler(options HandlerOptions) meta.WebHandler {
	if options.Heartbeat == 0 {
		options.Heartbeat = defaultHeartbeat
	}

	h := &handler{
		pool:        options.ProcessPool,
		heartbeat:   options.Heartbeat,
		compression: options.Compression,
		gzipLevel:   gzipLevel(options.CompressionLevel),
		metaOptions: options.MetaOptions,
		refusal:     options.Refusal,
		beat:        defaultBeat,
		ch:          make(chan error),
	}
	if options.HeartbeatEvent != "" {
		if strings.ContainsAny(options.HeartbeatEvent, "\r\n") {
			h.invalid = errors.New("sse: HeartbeatEvent must be a single line")
		}
		h.beatEvent = options.HeartbeatEvent
	}
	return h
}

// gzipLevel keeps compress/gzip inside this file: the option speaks the vocabulary of the
// framework, not of the algorithm behind it.
func gzipLevel(level gen.CompressionLevel) int {
	switch level {
	case gen.CompressionBestSpeed:
		return gzip.BestSpeed
	case gen.CompressionBestSize:
		return gzip.BestCompression
	}
	return gzip.DefaultCompression
}

// HandlerOptions defines options for the SSE handler
type HandlerOptions struct {
	ProcessPool []gen.Atom // Worker processes for handling connections (round-robin)

	// Heartbeat is how long a stream may stay quiet before a keepalive is written. Written
	// only when nothing else was, and checked on a fixed tick, so a stream can go silent
	// for up to twice this. Zero means 30s, negative disables it.
	Heartbeat time.Duration

	// HeartbeatEvent is the name to send the keepalive under, which makes it visible to
	// the clients subscribed to that name and to no one else. Empty sends a comment, which
	// no client sees at all. One line: CR and LF are refused at start.
	HeartbeatEvent string

	Compression bool // Enable gzip compression for clients that support it

	// CompressionLevel is the trade-off when Compression is on. Zero means default.
	CompressionLevel gen.CompressionLevel

	// MetaOptions is how the meta process of one connection is spawned. MailboxSize bounds
	// it: the writer there is a socket, so a slow client makes it grow.
	MetaOptions gen.MetaOptions

	// Refusal answers a request this handler could not turn into a stream. Nil answers with
	// plain text, which a caller speaking another protocol cannot read.
	Refusal RefusalHandler
}

// RefusalHandler answers a request the handler could not accept. Nothing has been written yet,
// so it owns the whole response: headers, status and body. The status is the one the handler
// would have used, and the reason is one of the sentinels below.
type RefusalHandler func(writer http.ResponseWriter, request *http.Request,
	status int, reason error)

var (
	ErrHandlerNotInitialized = errors.New("handler is not initialized")
	ErrHandlerTerminated     = errors.New("handler terminated")
	ErrNotAcceptable         = errors.New("this endpoint answers text/event-stream and the request does not accept it")
	ErrNoFlusher             = errors.New("streaming is not supported by this server")
	ErrConnectionRefused     = errors.New("unable to spawn the connection process")
)

type handler struct {
	gen.MetaProcess

	pool        []gen.Atom
	i           int32
	metaOptions gen.MetaOptions
	heartbeat   time.Duration
	beat        []byte
	beatEvent   string // empty: the beat is the comment in beat
	invalid     error
	compression bool
	gzipLevel   int
	refusal     RefusalHandler
	ch          chan error

	// Terminate runs on the Start goroutine or on the mailbox one, while ServeHTTP and
	// HandleInspect read this from theirs
	terminated atomic.Bool

	// counted on the http server goroutines, read by HandleInspect on the mailbox one
	connections   atomic.Int64
	open          atomic.Int64
	compressed    atomic.Int64
	unavailable   atomic.Int64
	notAcceptable atomic.Int64
	noFlusher     atomic.Int64
	spawnFailed   atomic.Int64
	lastConnect   atomic.Int64 // unix nano
}

//
// gen.MetaBehavior implementation
//

func (h *handler) Init(process gen.MetaProcess) error {
	if h.invalid != nil {
		return h.invalid
	}
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
	h.terminated.Store(true)
	h.ch <- reason
	close(h.ch)
}

const handlerInspectHelp = "summary keys: state, pool, heartbeat, compression, connections, open, " +
	"compressed, unavailable, not_acceptable, no_flusher, spawn_failed, last_connect"

func (h *handler) HandleInspect(from gen.PID, item ...string) map[string]string {
	if len(item) == 0 {
		return h.inspectSummary()
	}

	result := map[string]string{}
	for _, q := range item {
		if q == "help" {
			result["help"] = handlerInspectHelp
			continue
		}
		result[q] = "<unknown item>"
	}
	return result
}

func (h *handler) inspectSummary() map[string]string {
	state := "running"
	switch {
	case h.MetaProcess == nil:
		state = "not initialized"
	case h.terminated.Load():
		state = "terminated"
	}

	pool := "parent"
	if len(h.pool) > 0 {
		names := make([]string, 0, len(h.pool))
		for _, name := range h.pool {
			names = append(names, string(name))
		}
		pool = strings.Join(names, ",")
	}

	last := "never"
	if at := h.lastConnect.Load(); at > 0 {
		last = time.Since(time.Unix(0, at)).Round(time.Second).String()
	}

	return map[string]string{
		"state":          state,
		"pool":           pool,
		"heartbeat":      h.heartbeat.String(),
		"compression":    fmt.Sprintf("%t", h.compression),
		"connections":    fmt.Sprintf("%d", h.connections.Load()),
		"open":           fmt.Sprintf("%d", h.open.Load()),
		"compressed":     fmt.Sprintf("%d", h.compressed.Load()),
		"unavailable":    fmt.Sprintf("%d", h.unavailable.Load()),
		"not_acceptable": fmt.Sprintf("%d", h.notAcceptable.Load()),
		"no_flusher":     fmt.Sprintf("%d", h.noFlusher.Load()),
		"spawn_failed":   fmt.Sprintf("%d", h.spawnFailed.Load()),
		"last_connect":   last,
		"items":          "help",
	}
}

func (h *handler) refuse(writer http.ResponseWriter, request *http.Request,
	status int, reason error) {

	if h.refusal == nil {
		http.Error(writer, reason.Error(), status)
		return
	}
	h.refusal(writer, request, status, reason)
}

//
// http.Handler implementation
//

func (h *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if h.MetaProcess == nil {
		h.unavailable.Add(1)
		h.refuse(writer, request, http.StatusServiceUnavailable, ErrHandlerNotInitialized)
		return
	}

	if h.terminated.Load() {
		h.unavailable.Add(1)
		h.refuse(writer, request, http.StatusServiceUnavailable, ErrHandlerTerminated)
		return
	}

	if acceptsStream(request.Header.Get("Accept")) == false {
		h.notAcceptable.Add(1)
		h.refuse(writer, request, http.StatusNotAcceptable, ErrNotAcceptable)
		return
	}

	flusher, ok := writer.(http.Flusher)
	if ok == false {
		h.noFlusher.Add(1)
		h.refuse(writer, request, http.StatusInternalServerError, ErrNoFlusher)
		return
	}

	// set SSE headers
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	c := &serverConnection{
		writer:     writer,
		rawFlusher: flusher,
		request:    request,
		heartbeat:  h.heartbeat,
		beat:       h.beat,
		beatEvent:  h.beatEvent,
		done:       make(chan struct{}),
	}

	// negotiate gzip compression
	if h.compression == true && strings.Contains(request.Header.Get("Accept-Encoding"), "gzip") {
		writer.Header().Set("Content-Encoding", "gzip")
		gz, _ := gzip.NewWriterLevel(writer, h.gzipLevel)
		c.writer = gz
		c.gzWriter = gz
	}

	if l := len(h.pool); l > 0 {
		i := int(atomic.AddInt32(&h.i, 1))
		c.process = h.pool[i%l]
	}

	if _, err := h.Spawn(c, h.metaOptions); err != nil {
		h.spawnFailed.Add(1)
		// the stream headers are already on the writer, and a refusal is not a stream: left
		// there, Content-Encoding alone would make the body unreadable
		for _, name := range []string{"Content-Type", "Cache-Control", "Connection",
			"X-Accel-Buffering", "Content-Encoding"} {
			writer.Header().Del(name)
		}
		h.refuse(writer, request, http.StatusInternalServerError, ErrConnectionRefused)
		h.Log().Error("unable to spawn SSE connection meta process: %s", err)
		return
	}

	h.connections.Add(1)
	h.lastConnect.Store(time.Now().UnixNano())
	if c.gzWriter != nil {
		h.compressed.Add(1)
	}
	h.open.Add(1)
	defer h.open.Add(-1)

	// block until connection is done
	<-c.done
}

//
// serverConnection - meta-process for each SSE connection
//

// internal self-messages keep the mailbox goroutine the only writer
type messageInit struct{}

type messageShutdown struct{}

type messageHeartbeat struct{}

type serverConnection struct {
	gen.MetaProcess

	writer     io.Writer
	rawFlusher http.Flusher
	gzWriter   *gzip.Writer // nil when compression disabled
	process    gen.Atom
	request    *http.Request
	heartbeat  time.Duration
	beat       []byte
	beatEvent  string
	done       chan struct{}
	started    time.Time
	bytesOut   uint64
	// written in HandleMessage and read in HandleInspect, both on the mailbox goroutine
	messages    uint64
	lastMessage int64 // unix nano
	writeFailed uint64
	heartbeats  uint64
	lastWrite   time.Time
	stopBeat    gen.CancelFunc
	terminated  atomic.Bool
}

func (c *serverConnection) Init(process gen.MetaProcess) error {
	c.MetaProcess = process
	c.started = time.Now()
	if len(c.beat) == 0 {
		c.beat = defaultBeat
	}

	if c.heartbeat > 0 {
		stop, err := c.SendEvery(process.ID(), messageHeartbeat{}, c.heartbeat)
		if err != nil {
			return err
		}
		c.stopBeat = stop
	}
	return nil
}

func (c *serverConnection) Start() error {
	id := c.ID()

	var to any = c.Parent()
	if c.process != "" {
		to = c.process
	}

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

	// initial heartbeat is written by the mailbox goroutine (the only writer)
	c.Send(id, messageInit{})

	// wait for client disconnect (request context is cancelled on disconnect)
	<-c.request.Context().Done()

	// hand teardown to the mailbox goroutine; it terminates the meta and
	// Terminate closes done. if the mailbox is unreachable, returning here
	// terminates the meta instead.
	if c.Send(id, messageShutdown{}) != nil {
		return nil
	}
	<-c.done

	return nil
}

func (c *serverConnection) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case messageInit:
		// initial heartbeat comment to establish the stream
		if _, err := c.writer.Write([]byte(": connected\n\n")); err != nil {
			return err
		}
		c.flush()
		c.lastWrite = time.Now()

	case messageHeartbeat:
		// a stream that is sending data needs no keepalive
		if time.Since(c.lastWrite) < c.heartbeat {
			return nil
		}
		if _, err := c.writer.Write(c.beatFrame()); err != nil {
			c.writeFailed++
			return err
		}
		c.flush()
		c.heartbeats++
		c.lastWrite = time.Now()

	case Message:
		data := formatSSE(m)
		if _, err := c.writer.Write(data); err != nil {
			c.writeFailed++
			c.Log().Error("unable to write SSE data: %s", err)
			return err
		}
		c.flush()
		atomic.AddUint64(&c.bytesOut, uint64(len(data)))
		c.messages++
		c.lastMessage = time.Now().UnixNano()
		c.lastWrite = time.Now()

	case messageShutdown:
		// teardown here so it is serialized with writes; the returned reason
		// ends the mailbox loop, so nothing writes after this
		if c.gzWriter != nil {
			c.gzWriter.Close()
		}
		var to any = c.Parent()
		if c.process != "" {
			to = c.process
		}
		if err := c.Send(to, MessageDisconnect{ID: c.ID()}); err != nil {
			c.Log().Error("unable to send sse.MessageDisconnect: %s", err)
		}
		return gen.TerminateReasonNormal

	default:
		c.Log().Error("unsupported message from %s. ignored", from)
	}
	return nil
}

func (c *serverConnection) flush() {
	if c.gzWriter != nil {
		c.gzWriter.Flush()
	}
	c.rawFlusher.Flush()
}

func (c *serverConnection) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	return gen.ErrUnsupported, nil
}

func (c *serverConnection) Terminate(reason error) {
	c.terminated.Store(true)
	if c.stopBeat != nil {
		c.stopBeat()
	}

	// the framework calls Terminate exactly once on any termination path
	// (Start return, HandleMessage error, panic recover), so done is closed once
	close(c.done)

	if reason == nil || reason == gen.TerminateReasonNormal {
		return
	}
	c.Log().Error("terminated abnormally: %s", reason)
}

const connectionInspectHelp = "summary keys: state, process, remote, local, uptime, heartbeat, " +
	"compression, messages, bytes_out, write_failed, heartbeats, last_message, last_event_id"

func (c *serverConnection) HandleInspect(from gen.PID, item ...string) map[string]string {
	if len(item) == 0 {
		return c.inspectSummary()
	}

	result := map[string]string{}
	for _, q := range item {
		if q == "help" {
			result["help"] = connectionInspectHelp
			continue
		}
		result[q] = "<unknown item>"
	}
	return result
}

func (c *serverConnection) inspectSummary() map[string]string {
	state := "streaming"
	if c.terminated.Load() {
		state = "closed"
	}

	// an empty pool means the parent handles this connection
	target := "not set"
	switch {
	case c.process != "":
		target = string(c.process)
	case c.MetaProcess != nil:
		target = c.Parent().String()
	}

	uptime := "never started"
	if c.started.IsZero() == false {
		uptime = time.Since(c.started).Round(time.Second).String()
	}

	last := "never"
	if c.lastMessage > 0 {
		last = time.Since(time.Unix(0, c.lastMessage)).Round(time.Second).String()
	}

	result := map[string]string{
		"state":        state,
		"process":      target,
		"uptime":       uptime,
		"heartbeat":    c.heartbeat.String(),
		"compression":  fmt.Sprintf("%t", c.gzWriter != nil),
		"messages":     fmt.Sprintf("%d", c.messages),
		"bytes_out":    fmt.Sprintf("%d", atomic.LoadUint64(&c.bytesOut)),
		"write_failed": fmt.Sprintf("%d", c.writeFailed),
		"heartbeats":   fmt.Sprintf("%d", c.heartbeats),
		"last_message": last,
		"items":        "help",
	}

	if c.request == nil {
		return result
	}
	if c.request.RemoteAddr != "" {
		result["remote"] = c.request.RemoteAddr
	}
	if c.request.Context() != nil {
		if local, ok := c.request.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			result["local"] = local.String()
		}
	}
	if id := c.request.Header.Get("Last-Event-ID"); id != "" {
		result["last_event_id"] = id
	}
	return result
}

// beatFrame is the keepalive: a named event when one was asked for, a comment otherwise.
// The clock is the data such an event has to carry - a client drops one that has none.
func (c *serverConnection) beatFrame() []byte {
	if c.beatEvent == "" {
		return c.beat
	}
	millis := strconv.FormatInt(time.Now().UnixMilli(), 10)
	return formatSSE(Message{Event: c.beatEvent, Data: []byte(millis)})
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

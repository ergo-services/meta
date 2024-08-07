package websocket

import (
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
	ws "github.com/gorilla/websocket"
)

func CreateConnection(options ConnectionOptions) (gen.MetaBehavior, error) {
	if options.HandshakeTimeout == 0 {
		options.HandshakeTimeout = 15 * time.Second
	}
	dialer := &ws.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  options.HandshakeTimeout,
		EnableCompression: options.EnableCompression,
	}
	c, _, err := dialer.Dial(options.URL.String(), nil)
	if err != nil {
		return nil, err
	}

	return &connection{
		process: options.Process,
		conn:    c,
	}, nil
}

type ConnectionOptions struct {
	Process           gen.Atom
	URL               url.URL
	HandshakeTimeout  time.Duration
	EnableCompression bool
}

//
// Connection gen.MetaBehavior implementation
//

type connection struct {
	gen.MetaProcess
	conn     *ws.Conn
	process  gen.Atom
	bytesIn  uint64
	bytesOut uint64
}

func (c *connection) Init(process gen.MetaProcess) error {
	c.MetaProcess = process
	return nil
}

func (c *connection) Start() error {
	var to any

	id := c.ID()

	if c.process == "" {
		to = c.Parent()
	} else {
		to = c.process
	}

	defer func() {
		c.conn.Close()
		message := MessageDisconnect{
			ID: id,
		}
		if err := c.Send(to, message); err != nil {
			c.Log().Error("unable to send websocket.MessageDisconnect: %s", err)
			return
		}
	}()

	message := MessageConnect{
		ID:         id,
		RemoteAddr: c.conn.RemoteAddr(),
		LocalAddr:  c.conn.LocalAddr(),
	}
	if err := c.Send(to, message); err != nil {
		c.Log().Error("unable to send websocket.MessageConnect to %v: %s", to, err)
		return err
	}

	for {
		mt, m, err := c.conn.ReadMessage()
		if err != nil {
			if ws.IsCloseError(err, ws.CloseNormalClosure) || ws.IsCloseError(err, ws.CloseGoingAway) {
				return nil
			}
			return err
		}
		message := Message{
			ID:   id,
			Type: MessageType(mt),
			Body: m,
		}
		atomic.AddUint64(&c.bytesIn, uint64(len(m)))
		if err := c.Send(to, message); err != nil {
			c.Log().Error("unable to send websocket.Message: %s", err)
			return err
		}
	}
}

func (c *connection) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case Message:
		if m.Type == 0 {
			m.Type = MessageTypeText
		}
		if err := c.conn.WriteMessage(int(m.Type), m.Body); err != nil {
			c.Log().Error("unable to write data into the web socket: %s", err)
			return err
		}
		atomic.AddUint64(&c.bytesOut, uint64(len(m.Body)))

	default:
		c.Log().Error("unsupported message from %s. ignored", from)
	}
	return nil
}

func (w *connection) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	return gen.ErrUnsupported, nil
}

func (c *connection) Terminate(reason error) {
	c.conn.Close()
	if reason == nil || reason == gen.TerminateReasonNormal {
		return
	}
	c.Log().Error("terminated abnormaly: %s", reason)
}

func (c *connection) HandleInspect(from gen.PID, item ...string) map[string]string {
	bytesIn := atomic.LoadUint64(&c.bytesIn)
	bytesOut := atomic.LoadUint64(&c.bytesOut)
	return map[string]string{
		"local":     c.conn.LocalAddr().String(),
		"remote":    c.conn.RemoteAddr().String(),
		"process":   c.process.String(),
		"bytes in":  fmt.Sprintf("%d", bytesIn),
		"bytes out": fmt.Sprintf("%d", bytesOut),
	}
}

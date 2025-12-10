package sse

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"ergo.services/ergo/gen"
)

const (
	defaultReconnectInterval = 3 * time.Second
)

// CreateConnection creates a new SSE client connection meta-process.
// The connection is not established until the meta-process is spawned and started.
func CreateConnection(options ConnectionOptions) gen.MetaBehavior {
	if options.ReconnectInterval == 0 {
		options.ReconnectInterval = defaultReconnectInterval
	}
	return &clientConnection{
		options:     options,
		lastEventID: options.LastEventID,
	}
}

// ConnectionOptions defines options for SSE client connection
type ConnectionOptions struct {
	Process           gen.Atom      // Target process for messages (uses parent if empty)
	URL               url.URL       // SSE endpoint URL
	Headers           http.Header   // Custom HTTP headers
	LastEventID       string        // Initial Last-Event-ID for reconnection
	ReconnectInterval time.Duration // Default reconnect delay (can be overridden by server)
}

type clientConnection struct {
	gen.MetaProcess

	options           ConnectionOptions
	process           gen.Atom
	lastEventID       string
	reconnectInterval time.Duration
	bytesIn           uint64
	response          *http.Response
}

func (c *clientConnection) Init(process gen.MetaProcess) error {
	c.MetaProcess = process
	c.process = c.options.Process
	c.reconnectInterval = c.options.ReconnectInterval
	return nil
}

func (c *clientConnection) Start() error {
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
	}()

	client := &http.Client{}

	req, err := http.NewRequest("GET", c.options.URL.String(), nil)
	if err != nil {
		c.Log().Error("unable to create request: %s", err)
		return err
	}

	// set SSE headers
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	// set custom headers
	for key, values := range c.options.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	if c.lastEventID != "" {
		req.Header.Set("Last-Event-ID", c.lastEventID)
	}

	// make request
	resp, err := client.Do(req)
	if err != nil {
		c.Log().Error("unable to connect: %s", err)
		return err
	}
	c.response = resp
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.Log().Error("unexpected status code: %d", resp.StatusCode)
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "text/event-stream") == false {
		c.Log().Error("unexpected content type: %s", contentType)
		return fmt.Errorf("unexpected content type: %s", contentType)
	}

	// send connect message
	connectMsg := MessageConnect{
		ID: id,
	}
	if resp.Request != nil {
		connectMsg.Request = resp.Request
	}

	if resp.Request != nil && resp.Request.RemoteAddr != "" {
		connectMsg.RemoteAddr = &addr{network: "tcp", address: resp.Request.RemoteAddr}
	}

	if err := c.Send(to, connectMsg); err != nil {
		c.Log().Error("unable to send sse.MessageConnect to %v: %s", to, err)
		return err
	}

	// parse SSE stream
	return c.parseStream(bufio.NewReader(resp.Body), to, id)
}

func (c *clientConnection) parseStream(reader *bufio.Reader, to any, id gen.Alias) error {
	var event, data, msgID string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		atomic.AddUint64(&c.bytesIn, uint64(len(line)))

		line = strings.TrimRight(line, "\r\n")

		// empty line means dispatch event
		if line == "" {
			if data != "" {
				// remove trailing newline from data
				data = strings.TrimSuffix(data, "\n")

				msg := Message{
					ID:    id,
					Event: event,
					Data:  []byte(data),
					MsgID: msgID,
				}
				if err := c.Send(to, msg); err != nil {
					c.Log().Error("unable to send sse.Message: %s", err)
					return err
				}
			}
			// reset for next event
			event, data, msgID = "", "", ""
			continue
		}

		// skip comments
		if strings.HasPrefix(line, ":") {
			continue
		}

		// parse field
		field, value := parseField(line)
		switch field {
		case "event":
			event = value
		case "data":
			data += value + "\n"
		case "id":
			if strings.ContainsRune(value, '\x00') == false {
				msgID = value
				c.lastEventID = value
			}
		case "retry":
			if ms, err := strconv.Atoi(value); err == nil && ms >= 0 {
				c.reconnectInterval = time.Duration(ms) * time.Millisecond
			}
		}
	}
}

func parseField(line string) (field, value string) {
	colonIndex := strings.Index(line, ":")
	if colonIndex == -1 {
		// no value
		return line, ""
	}

	field = line[:colonIndex]
	value = line[colonIndex+1:]

	// remove leading space from value if present
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}

	return field, value
}

func (c *clientConnection) HandleMessage(from gen.PID, message any) error {
	// SSE is unidirectional - client cannot send messages to server
	c.Log().Error("SSE connection is read-only, message from %s ignored", from)
	return nil
}

func (c *clientConnection) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	return gen.ErrUnsupported, nil
}

func (c *clientConnection) Terminate(reason error) {
	// close the response body to interrupt the read loop
	if c.response != nil {
		c.response.Body.Close()
	}
	if reason == nil || reason == gen.TerminateReasonNormal {
		return
	}
	c.Log().Error("terminated abnormally: %s", reason)
}

func (c *clientConnection) HandleInspect(from gen.PID, item ...string) map[string]string {
	bytesIn := atomic.LoadUint64(&c.bytesIn)
	return map[string]string{
		"url":             c.options.URL.String(),
		"process":         c.process.String(),
		"bytes in":        fmt.Sprintf("%d", bytesIn),
		"last event id":   c.lastEventID,
		"reconnect delay": c.reconnectInterval.String(),
	}
}

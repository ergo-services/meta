package sse

import (
	"net"
	"net/http"

	"ergo.services/ergo/gen"
)

// MessageConnect is sent when SSE connection is established
type MessageConnect struct {
	ID         gen.Alias
	RemoteAddr net.Addr
	LocalAddr  net.Addr
	Request    *http.Request
}

// MessageDisconnect is sent when SSE connection is closed
type MessageDisconnect struct {
	ID gen.Alias
}

// Message represents an SSE event
type Message struct {
	ID    gen.Alias // Connection identifier for routing replies
	Event string    // Event type (optional, omitted if empty)
	Data  []byte    // Event data (can be multi-line)
	MsgID string    // Event ID for client reconnection (optional)
	Retry int       // Retry hint in milliseconds (optional, server-side only)
}

// MessageLastEventID is sent when client reconnects with Last-Event-ID header
type MessageLastEventID struct {
	ID          gen.Alias
	LastEventID string
}

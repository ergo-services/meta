package websocket

import (
	"net"

	"ergo.services/ergo/gen"
)

type MessageConnect struct {
	ID         gen.Alias
	RemoteAddr net.Addr
	LocalAddr  net.Addr
}

type MessageDisconnect struct {
	ID gen.Alias
}

type MessageType int
type Message struct {
	ID   gen.Alias
	Type MessageType
	Body []byte
}

const (
	MessageTypeText   MessageType = 1
	MessageTypeBinary MessageType = 2
	MessageTypeClose  MessageType = 8
	MessageTypePing   MessageType = 9
	MessageTypePong   MessageType = 10
)

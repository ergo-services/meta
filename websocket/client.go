package websocket

import (
	"net/http"
	"net/url"
	"time"

	"ergo.services/ergo/gen"
	ws "github.com/gorilla/websocket"
)

func CreateClient(options ClientOptions) (gen.MetaBehavior, error) {
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

	return &wsc{
		process: options.Process,
		conn:    c,
	}, nil
}

type ClientOptions struct {
	Process           gen.Atom
	URL               url.URL
	HandshakeTimeout  time.Duration
	EnableCompression bool
}

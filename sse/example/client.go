package main

import (
	"net/url"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/node"

	"ergo.services/meta/sse"
)

func main() {

	opts := gen.NodeOptions{}
	opts.Log.Level = gen.LogLevelInfo

	n, err := node.Start("sse_client@localhost", opts, gen.Version{})
	if err != nil {
		panic(err)
	}

	n.Log().Info("Connecting to http://localhost:8080/events")
	n.Log().Info("Press Ctrl+C to stop")

	// spawn SSE client process
	_, err = n.Spawn(factoryClient, gen.ProcessOptions{})
	if err != nil {
		panic(err)
	}

	n.Wait()
}

func factoryClient() gen.ProcessBehavior {
	return &clientProcess{}
}

type clientProcess struct {
	act.Actor
	metaID gen.Alias
}

func (c *clientProcess) Init(args ...any) error {
	// create SSE connection
	opts := sse.ConnectionOptions{
		URL: url.URL{
			Scheme: "http",
			Host:   "localhost:8080",
			Path:   "/events",
		},
	}

	conn := sse.CreateConnection(opts)
	id, err := c.SpawnMeta(conn, gen.MetaOptions{})
	if err != nil {
		return err
	}
	c.metaID = id

	return nil
}

func (c *clientProcess) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case sse.MessageConnect:
		c.Log().Info("[CONNECTED] Connection ID: %s", m.ID)

	case sse.MessageDisconnect:
		c.Log().Info("[DISCONNECTED] Connection ID: %s", m.ID)

	case sse.Message:
		if m.Event == "" {
			c.Log().Info("[MESSAGE] id=%s data=%s", m.MsgID, string(m.Data))
		} else {
			c.Log().Info("[%s] id=%s data=%s", m.Event, m.MsgID, string(m.Data))
		}
	}

	return nil
}

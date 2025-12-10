package main

import (
	"fmt"
	"net/http"
	"time"

	"ergo.services/ergo/act"
	"ergo.services/ergo/gen"
	"ergo.services/ergo/meta"
	"ergo.services/ergo/node"

	"ergo.services/meta/sse"
)

func main() {

	opts := gen.NodeOptions{}
	opts.Log.Level = gen.LogLevelInfo

	n, err := node.Start("sse_server@localhost", opts, gen.Version{})
	if err != nil {
		panic(err)
	}

	_, err = n.Spawn(factoryWeb, gen.ProcessOptions{})
	if err != nil {
		panic(err)
	}

	n.Log().Info("Server started on http://localhost:8080")
	n.Log().Info("Endpoints:")
	n.Log().Info("  /         - HTML page with SSE client")
	n.Log().Info("  /events   - SSE endpoint")
	n.Log().Info("Press Ctrl+C to stop")

	n.Wait()
}

func factoryWeb() gen.ProcessBehavior {
	return &webProcess{}
}

type webProcess struct {
	act.Actor
	connections map[gen.Alias]bool
	counter     int
}

func (w *webProcess) Init(args ...any) error {
	w.connections = make(map[gen.Alias]bool)

	mux := http.NewServeMux()

	// SSE handler
	sseHandler := sse.CreateHandler(sse.HandlerOptions{
		Heartbeat: 15 * time.Second,
	})
	if _, err := w.SpawnMeta(sseHandler, gen.MetaOptions{}); err != nil {
		return err
	}
	mux.Handle("/events", sseHandler)

	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/html")
		rw.Write([]byte(htmlPage))
	})

	// create web server
	serverOpts := meta.WebServerOptions{
		Host:    "localhost",
		Port:    8080,
		Handler: mux,
	}
	webServer, err := meta.CreateWebServer(serverOpts)
	if err != nil {
		return err
	}
	if _, err := w.SpawnMeta(webServer, gen.MetaOptions{}); err != nil {
		return err
	}

	// start periodic event sender
	w.Send(w.PID(), "tick")

	return nil
}

func (w *webProcess) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case sse.MessageConnect:
		w.Log().Info("New SSE connection: %s (remote: %s)", m.ID, m.RemoteAddr)
		w.connections[m.ID] = true

		// send welcome message
		welcome := sse.Message{
			Event: "welcome",
			Data:  []byte(fmt.Sprintf("Connected! You are client #%d", len(w.connections))),
			MsgID: "0",
		}
		w.SendAlias(m.ID, welcome)

	case sse.MessageDisconnect:
		w.Log().Info("SSE disconnected: %s", m.ID)
		delete(w.connections, m.ID)

	case sse.MessageLastEventID:
		w.Log().Info("Client reconnected with Last-Event-ID: %s", m.LastEventID)

	case string:
		if m == "tick" {
			// send periodic event to all connections
			w.counter++
			now := time.Now().Format("15:04:05")

			for connID := range w.connections {
				msg := sse.Message{
					Event: "time",
					Data:  []byte(fmt.Sprintf(`{"counter": %d, "time": "%s", "clients": %d}`, w.counter, now, len(w.connections))),
					MsgID: fmt.Sprintf("%d", w.counter),
				}
				if err := w.SendAlias(connID, msg); err != nil {
					w.Log().Error("Failed to send to %s: %s", connID, err)
				}
			}

			// schedule next tick
			w.SendAfter(w.PID(), "tick", 2*time.Second)
		}
	}

	return nil
}

const htmlPage = `<!DOCTYPE html>
<html>
<head>
    <title>SSE Example</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
            max-width: 800px;
            margin: 50px auto;
            padding: 20px;
            background: #1a1a2e;
            color: #eee;
        }
        h1 { color: #00d9ff; }
        .status {
            padding: 10px 20px;
            border-radius: 5px;
            margin: 20px 0;
            font-weight: bold;
        }
        .connected { background: #0a3d0a; color: #4caf50; }
        .disconnected { background: #3d0a0a; color: #f44336; }
        .info {
            background: #16213e;
            padding: 20px;
            border-radius: 10px;
            margin: 20px 0;
        }
        .info h3 { margin-top: 0; color: #00d9ff; }
        .counter { font-size: 48px; color: #00d9ff; }
        #events {
            background: #0f0f23;
            border: 1px solid #333;
            border-radius: 5px;
            padding: 15px;
            height: 300px;
            overflow-y: auto;
            font-family: monospace;
            font-size: 13px;
        }
        .event { margin: 5px 0; padding: 5px; border-left: 3px solid #00d9ff; padding-left: 10px; }
        .event-welcome { border-color: #4caf50; }
        .event-time { border-color: #ff9800; }
        .timestamp { color: #666; }
    </style>
</head>
<body>
    <h1>SSE Server Example</h1>

    <div id="status" class="status disconnected">Disconnected</div>

    <div class="info">
        <h3>Live Data</h3>
        <div>Counter: <span id="counter" class="counter">-</span></div>
        <div>Server Time: <span id="time">-</span></div>
        <div>Connected Clients: <span id="clients">-</span></div>
    </div>

    <h3>Event Log</h3>
    <div id="events"></div>

    <script>
        const statusEl = document.getElementById('status');
        const eventsEl = document.getElementById('events');
        const counterEl = document.getElementById('counter');
        const timeEl = document.getElementById('time');
        const clientsEl = document.getElementById('clients');

        function connect() {
            const es = new EventSource('/events');

            es.onopen = () => {
                statusEl.textContent = 'Connected';
                statusEl.className = 'status connected';
                addEvent('system', 'Connection established');
            };

            es.onerror = () => {
                statusEl.textContent = 'Disconnected (reconnecting...)';
                statusEl.className = 'status disconnected';
                addEvent('system', 'Connection lost');
            };

            es.addEventListener('welcome', (e) => {
                addEvent('welcome', e.data);
            });

            es.addEventListener('time', (e) => {
                const data = JSON.parse(e.data);
                counterEl.textContent = data.counter;
                timeEl.textContent = data.time;
                clientsEl.textContent = data.clients;
                addEvent('time', e.data);
            });
        }

        function addEvent(type, data) {
            const now = new Date().toLocaleTimeString();
            const div = document.createElement('div');
            div.className = 'event event-' + type;
            div.innerHTML = '<span class="timestamp">[' + now + ']</span> <strong>' + type + ':</strong> ' + data;
            eventsEl.insertBefore(div, eventsEl.firstChild);

            // Keep only last 50 events
            while (eventsEl.children.length > 50) {
                eventsEl.removeChild(eventsEl.lastChild);
            }
        }

        connect();
    </script>
</body>
</html>`

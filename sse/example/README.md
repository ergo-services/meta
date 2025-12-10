# SSE Example

This example demonstrates Server-Sent Events (SSE) meta-process implementation for Ergo Framework.

## Server

Starts HTTP server on `localhost:8080` with:
- `/` - HTML page with embedded SSE client (view in browser)
- `/events` - SSE endpoint

The server sends periodic events every 2 seconds with counter, server time, and connected clients count.

```bash
go run ./server.go
```

Open http://localhost:8080 in browser to see live updates.

## Client

Connects to SSE endpoint and prints received events to console.

```bash
go run ./client.go
```

## Message Types

- `sse.MessageConnect` - connection established
- `sse.MessageDisconnect` - connection closed
- `sse.Message` - SSE event with Event, Data, MsgID, Retry fields
- `sse.MessageLastEventID` - client reconnected with Last-Event-ID header

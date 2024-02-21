package websocket

import (
	"ergo.services/ergo/gen"
	ws "github.com/gorilla/websocket"
)

//
// Connection gen.MetaBehavior implementation
//

type wsc struct {
	gen.MetaProcess
	process gen.Atom
	conn    *ws.Conn
}

func (w *wsc) Init(process gen.MetaProcess) error {
	w.MetaProcess = process
	return nil
}

func (w *wsc) Start() error {
	var to any

	id := w.ID()

	if w.process == "" {
		to = w.Parent()
	} else {
		to = w.process
	}

	defer func() {
		w.conn.Close()
		message := MessageDisconnect{
			ID: id,
		}
		if err := w.Send(to, message); err != nil {
			w.Log().Error("unable to send websocket.MessageDisconnect: %s", err)
			return
		}
	}()

	message := MessageConnect{
		ID:         id,
		RemoteAddr: w.conn.RemoteAddr(),
		LocalAddr:  w.conn.LocalAddr(),
	}
	if err := w.Send(to, message); err != nil {
		w.Log().Error("unable to send websocket.MessageConnect to %v: %s", to, err)
		return err
	}

	for {
		mt, m, err := w.conn.ReadMessage()
		if err != nil {
			if ws.IsCloseError(err, ws.CloseNormalClosure) || ws.IsCloseError(err, ws.CloseGoingAway) {
				return nil
			}
			w.Log().Error("unable to read from web socket: %s", err)
			return err
		}
		message := Message{
			ID:   id,
			Type: MessageType(mt),
			Body: m,
		}
		if err := w.Send(to, message); err != nil {
			w.Log().Error("unable to send websocket.Message: %s", err)
			return err
		}
	}
}

func (w *wsc) HandleMessage(from gen.PID, message any) error {
	switch m := message.(type) {
	case Message:
		if m.Type == 0 {
			m.Type = MessageTypeText
		}
		if err := w.conn.WriteMessage(int(m.Type), m.Body); err != nil {
			w.Log().Error("unable to write data into the web socket: %s", err)
			return err
		}
	default:
		w.Log().Error("unsupported message from %s. ignored", from)
	}
	return nil
}

func (w *wsc) HandleCall(from gen.PID, ref gen.Ref, request any) (any, error) {
	return gen.ErrUnsupported, nil
}

func (w *wsc) Terminate(reason error) {
	w.conn.Close()
}

func (w *wsc) HandleInspect(from gen.PID, item ...string) map[string]string {
	return map[string]string{
		"local":  w.conn.LocalAddr().String(),
		"remote": w.conn.RemoteAddr().String(),
	}
}

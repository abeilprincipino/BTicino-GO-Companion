package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gobwas/ws"
)

const (
	maxWebSocketFrame = 64 << 10
	heartbeatTimeout  = 60 * time.Second
	writeTimeout      = 5 * time.Second
)

type clientSet struct {
	mu      sync.Mutex
	clients map[*client]struct{}
}
type client struct {
	conn net.Conn
	mu   sync.Mutex
}

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		return
	}

	client := &client{conn: conn}

	s.clients.add(client)
	s.logger.InfoContext(r.Context(), "websocket connected", "remote_addr", r.RemoteAddr)
	defer func() {
		s.clients.remove(client)
		_ = conn.Close()
		s.logger.InfoContext(r.Context(), "websocket disconnected", "remote_addr", r.RemoteAddr)
	}()

	if client.write(Message{Type: "state", Payload: mustJSON(s.currentState())}) != nil {
		return
	}

	lastPing := time.Now()
	for {
		if conn.SetReadDeadline(lastPing.Add(heartbeatTimeout)) != nil {
			return
		}

		data, opcode, err := readClientFrame(conn)
		if err != nil || opcode == ws.OpClose {
			return
		}

		if opcode == ws.OpPing {
			if client.writeFrame(ws.OpPong, data) != nil {
				return
			}

			continue
		}

		if opcode != ws.OpText {
			return
		}

		message, err := ParseMessage(data)
		if err != nil {
			s.logger.WarnContext(r.Context(), "invalid websocket message", "remote_addr", r.RemoteAddr)
			if client.write(Message{Type: "error", Payload: mustJSON(map[string]string{"code": "invalid_message", "message": "message is invalid"})}) != nil {
				return
			}

			continue
		}

		if message.Type == "ping" {
			lastPing = time.Now()
		}

		s.handleMessage(client, r, message)
	}
}

func (s *Server) handleMessage(client *client, request *http.Request, message Message) {
	switch message.Type {
	case "ping":
		_ = client.write(Message{Type: "pong", ID: message.ID})
	case "command":
		if message.Action == "state.get" {
			_ = client.write(commandResult(message.ID, s.currentState(), nil))
			return
		}

		if s.commands == nil {
			_ = client.write(commandResult(message.ID, nil, errors.New("unsupported action")))
			return
		}

		payload, err := s.commands.HandleCommand(request, Command{ID: message.ID, Action: message.Action, Payload: message.Payload})
		if err != nil {
			s.logger.WarnContext(request.Context(), "websocket command failed", "command_id", message.ID, "action", message.Action, "error", err)
		}
		_ = client.write(commandResult(message.ID, payload, err))
	}
}

func (s *Server) BroadcastState() {
	s.broadcast(Message{Type: "state", Payload: mustJSON(s.currentState())})
}

func (s *Server) BroadcastEvent(payload any) {
	s.broadcast(Message{Type: "event", Payload: mustJSON(payload)})
}

func (s *Server) BroadcastTrace(payload any) {
	s.broadcast(Message{Type: "trace", Payload: mustJSON(payload)})
}

func (s *Server) broadcast(message Message) {
	for _, client := range s.clients.all() {
		if client.write(message) != nil {
			s.clients.remove(client)
			_ = client.conn.Close()
		}
	}
}

func (set *clientSet) add(c *client) {
	set.mu.Lock()
	defer set.mu.Unlock()

	if set.clients == nil {
		set.clients = make(map[*client]struct{})
	}

	set.clients[c] = struct{}{}
}

func (set *clientSet) remove(client *client) {
	set.mu.Lock()
	defer set.mu.Unlock()

	delete(set.clients, client)
}

func (set *clientSet) all() []*client {
	set.mu.Lock()
	defer set.mu.Unlock()

	clients := make([]*client, 0, len(set.clients))
	for client := range set.clients {
		clients = append(clients, client)
	}

	return clients
}

func (client *client) write(message Message) error {
	return client.writeFrame(ws.OpText, mustJSON(message))
}

func (client *client) writeFrame(opcode ws.OpCode, payload []byte) error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if err := client.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	defer client.conn.SetWriteDeadline(time.Time{}) //nolint:errcheck // best-effort deadline reset

	return ws.WriteFrame(client.conn, ws.NewFrame(opcode, true, payload))
}

func readClientFrame(conn net.Conn) ([]byte, ws.OpCode, error) {
	header, err := ws.ReadHeader(conn)
	if err != nil {
		return nil, 0, err
	}

	if err := ws.CheckHeader(header, ws.StateServerSide); err != nil {
		return nil, 0, err
	}

	if !header.Fin || header.Length > maxWebSocketFrame {
		return nil, 0, ErrInvalidMessage
	}

	payload := make([]byte, header.Length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, 0, err
	}

	if header.Masked {
		ws.Cipher(payload, header.Mask, 0)
	}

	return payload, header.OpCode, nil
}

func commandResult(id string, payload any, err error) Message {
	if err != nil {
		ok := false
		return Message{Type: "command_result", ID: id, OK: &ok, Error: mustJSON(map[string]string{"code": "command_failed", "message": err.Error()})}
	}

	ok := true
	return Message{Type: "command_result", ID: id, OK: &ok, Payload: mustJSON(payload)}
}

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"error":{"code":"internal_error","message":"message could not be encoded"}}`)
	}

	return data
}

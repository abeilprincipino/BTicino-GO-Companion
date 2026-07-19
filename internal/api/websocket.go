package api

import (
	"bticino-go-companion/internal/media"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
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

	if client.write(Message{Type: "state", Payload: mustJSON(s.currentPayload())}) != nil {
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
	_ = request
	switch message.Type {
	case "ping":
		_ = client.write(Message{Type: "pong", ID: message.ID})
	}
}

func (s *Server) BroadcastState() {
	s.broadcast(Message{Type: "state", Payload: mustJSON(s.currentPayload())})
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

func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"error":{"code":"internal_error","message":"message could not be encoded"}}`)
	}

	return data
}

type webrtcOfferPayload struct {
	SessionID    string `json:"session_id"`
	EntrypointID string `json:"entrypoint_id"`
	Origin       string `json:"origin"`
	OfferSDP     string `json:"offer_sdp"`
}

type webrtcCandidatePayload struct {
	SessionID string             `json:"session_id"`
	Candidate media.ICECandidate `json:"candidate"`
}

type webrtcClosePayload struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

func (s *Server) webrtcWebsocket(w http.ResponseWriter, r *http.Request) {
	if s.webrtc == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "webrtc control is unavailable")
		return
	}

	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		return
	}
	client := &client{conn: conn}
	var sessionID string
	defer func() {
		if sessionID != "" {
			_ = s.webrtc.Close(sessionID)
			s.logger.InfoContext(r.Context(), "webrtc session closed", "session_id", sessionID, "reason", "socket_disconnected")
		}
		_ = conn.Close()
	}()

	for {
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

		message, err := parseWebRTCMessage(data)
		if err != nil {
			if client.write(Message{Type: "error", Payload: mustJSON(map[string]string{"code": "invalid_message", "message": "message is invalid"})}) != nil {
				return
			}
			continue
		}

		var response Message
		switch message.Type {
		case "offer":
			var payload webrtcOfferPayload
			if json.Unmarshal(message.Payload, &payload) != nil || strings.TrimSpace(payload.SessionID) == "" || strings.TrimSpace(payload.EntrypointID) == "" || strings.TrimSpace(payload.Origin) == "" || strings.TrimSpace(payload.OfferSDP) == "" || sessionID != "" {
				response = webrtcError(message.ID, "invalid_offer", "offer is invalid")
				break
			}
			answer, offerErr := s.webrtc.Offer(r.Context(), payload.SessionID, payload.EntrypointID, payload.OfferSDP)
			if offerErr != nil {
				response = webrtcError(message.ID, "offer_failed", offerErr.Error())
				break
			}
			sessionID = payload.SessionID
			s.logger.InfoContext(r.Context(), "webrtc offer received", "session_id", sessionID, "entrypoint_id", payload.EntrypointID, "origin", payload.Origin, "event", "offer_received")
			response = Message{Type: "answer", ID: message.ID, Payload: mustJSON(map[string]string{"session_id": sessionID, "answer_sdp": answer})}
		case "candidate":
			var payload webrtcCandidatePayload
			if json.Unmarshal(message.Payload, &payload) != nil || payload.SessionID != sessionID || strings.TrimSpace(payload.Candidate.Candidate) == "" {
				response = webrtcError(message.ID, "invalid_candidate", "candidate is invalid")
				break
			}
			if candidateErr := s.webrtc.AddICECandidate(sessionID, payload.Candidate); candidateErr != nil {
				response = webrtcError(message.ID, "candidate_failed", candidateErr.Error())
				break
			}
			s.logger.InfoContext(r.Context(), "webrtc candidate received", "session_id", sessionID, "event", "candidate_received")
			response = Message{Type: "ack", ID: message.ID}
		case "close":
			var payload webrtcClosePayload
			if json.Unmarshal(message.Payload, &payload) != nil || payload.SessionID != sessionID {
				response = webrtcError(message.ID, "invalid_close", "close is invalid")
				break
			}
			_ = s.webrtc.Close(sessionID)
			s.logger.InfoContext(r.Context(), "webrtc session closed", "session_id", sessionID, "reason", payload.Reason, "event", "close_requested")
			response = Message{Type: "ack", ID: message.ID}
			sessionID = ""
		default:
			response = webrtcError(message.ID, "invalid_message", "message is invalid")
		}
		if client.write(response) != nil {
			return
		}
	}
}

func parseWebRTCMessage(data []byte) (Message, error) {
	var message Message
	if json.Unmarshal(data, &message) != nil || message.ID == "" || len(message.Payload) == 0 {
		return Message{}, ErrInvalidMessage
	}
	if message.Type != "offer" && message.Type != "candidate" && message.Type != "close" {
		return Message{}, ErrInvalidMessage
	}
	return message, nil
}

func webrtcError(id, code, message string) Message {
	return Message{Type: "error", ID: id, Payload: mustJSON(map[string]string{"code": code, "message": message})}
}

package api

import (
	"bticino-go-companion/internal/media"
	"context"
	"encoding/json"
	"errors"
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
	heartbeatTimeout  = 90 * time.Second
	heartbeatInterval = 20 * time.Second
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
	disconnectReason := "client_closed"

	s.clients.add(client)

	s.logger.DebugContext(r.Context(), "websocket connected", "channel", "state", "client_ip", sourceIP(r))
	defer func() {
		s.clients.remove(client)

		_ = conn.Close()

		s.logger.DebugContext(r.Context(), "websocket disconnected", "channel", "state", "client_ip", sourceIP(r), "reason", disconnectReason)
	}()

	if client.write(Message{Type: "state", Payload: mustJSON(s.currentPayload())}) != nil {
		disconnectReason = "initial_state_write_failed"
		return
	}

	s.logger.DebugContext(r.Context(), "websocket ready", "channel", "state", "client_ip", sourceIP(r))

	lastActivity := time.Now()
	for {
		if conn.SetReadDeadline(lastActivity.Add(heartbeatTimeout)) != nil {
			return
		}

		data, opcode, err := readClientFrame(conn)
		if err != nil {
			var networkError net.Error
			if errors.As(err, &networkError) {
				disconnectReason = "heartbeat_timeout"
			} else {
				disconnectReason = "read_failed"
			}

			return
		}

		if opcode == ws.OpClose {
			disconnectReason = "client_closed"
			return
		}

		if opcode == ws.OpPing {
			lastActivity = time.Now()

			if client.writeFrame(ws.OpPong, data) != nil {
				disconnectReason = "pong_write_failed"
				return
			}

			continue
		}

		if opcode != ws.OpText {
			if opcode == ws.OpPong {
				lastActivity = time.Now()
				continue
			}

			disconnectReason = "unsupported_frame"

			return
		}

		message, err := ParseMessage(data)
		if err != nil {
			s.logger.DebugContext(r.Context(), "websocket message rejected", "channel", "state", "client_ip", sourceIP(r), "reason", "invalid_message")

			if client.write(Message{Type: "error", Payload: mustJSON(map[string]string{"code": "invalid_message", "message": "message is invalid"})}) != nil {
				return
			}

			continue
		}

		lastActivity = time.Now()

		s.handleMessage(client, r, message)
	}
}

func (s *Server) handleMessage(client *client, request *http.Request, message Message) {
	_ = request

	if message.Type == "ping" {
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

// CloseWebSockets unblocks upgraded handlers before HTTP server shutdown waits for them.
func (s *Server) CloseWebSockets() {
	for _, client := range s.clients.all() {
		_ = client.conn.Close()
	}

	for _, client := range s.webrtcClients.all() {
		_ = client.conn.Close()
	}
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
	SessionID    string            `json:"session_id"`
	EntrypointID string            `json:"entrypoint_id"`
	Origin       string            `json:"origin"`
	OfferSDP     string            `json:"offer_sdp"`
	ICEServers   []media.ICEServer `json:"ice_servers"`
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
	s.webrtcClients.add(client)

	var sessionID string

	lastActivity := time.Now()
	nextPing := lastActivity.Add(heartbeatInterval)

	defer func() {
		s.webrtcClients.remove(client)

		if sessionID != "" {
			_ = s.webrtc.Close(sessionID)
		}

		_ = conn.Close()
	}()

	for {
		deadline := lastActivity.Add(heartbeatTimeout)
		if nextPing.Before(deadline) {
			deadline = nextPing
		}

		if conn.SetReadDeadline(deadline) != nil {
			return
		}

		data, opcode, err := readClientFrame(conn)
		if err != nil {
			var networkError net.Error
			if errors.As(err, &networkError) {
				now := time.Now()
				if now.Sub(lastActivity) >= heartbeatTimeout || client.writeFrame(ws.OpPing, nil) != nil {
					return
				}

				nextPing = now.Add(heartbeatInterval)

				continue
			}

			return
		}

		if opcode == ws.OpClose {
			return
		}

		if opcode == ws.OpPing {
			lastActivity = time.Now()
			nextPing = lastActivity.Add(heartbeatInterval)

			if client.writeFrame(ws.OpPong, data) != nil {
				return
			}

			continue
		}

		if opcode == ws.OpPong {
			lastActivity = time.Now()
			nextPing = lastActivity.Add(heartbeatInterval)

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

		lastActivity = time.Now()
		nextPing = lastActivity.Add(heartbeatInterval)

		var response Message

		sessionID, response = s.webrtcResponse(r.Context(), message, sessionID)
		if client.write(response) != nil {
			return
		}
	}
}

func (s *Server) webrtcResponse(ctx context.Context, message Message, sessionID string) (string, Message) {
	var response Message

	switch message.Type {
	case "offer":
		var payload webrtcOfferPayload
		if json.Unmarshal(message.Payload, &payload) != nil || strings.TrimSpace(payload.SessionID) == "" || strings.TrimSpace(payload.EntrypointID) == "" || strings.TrimSpace(payload.Origin) == "" || strings.TrimSpace(payload.OfferSDP) == "" || sessionID != "" {
			s.logger.DebugContext(ctx, "webrtc offer rejected", "reason", "invalid_offer")

			response = webrtcError(message.ID, "invalid_offer", "offer is invalid")

			break
		}

		offerCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		// Home Assistant's signaling client expects one response for every
		// request, so server candidates must be part of the SDP answer.
		answer, offerErr := s.webrtc.Offer(offerCtx, payload.SessionID, payload.EntrypointID, payload.OfferSDP, payload.ICEServers, nil)

		cancel()

		if offerErr != nil {
			s.logger.WarnContext(ctx, "webrtc offer failed", "session_id", payload.SessionID, "entrypoint_id", payload.EntrypointID, "error", offerErr)
			response = webrtcError(message.ID, "offer_failed", offerErr.Error())

			break
		}

		sessionID = payload.SessionID
		s.logger.DebugContext(ctx, "webrtc offer accepted", "session_id", sessionID, "entrypoint_id", payload.EntrypointID, "origin", payload.Origin)

		response = Message{Type: "answer", ID: message.ID, Payload: mustJSON(map[string]string{"session_id": sessionID, "answer_sdp": answer})}
	case "candidate":
		var payload webrtcCandidatePayload
		if json.Unmarshal(message.Payload, &payload) != nil || payload.SessionID != sessionID || strings.TrimSpace(payload.Candidate.Candidate) == "" {
			response = webrtcError(message.ID, "invalid_candidate", "candidate is invalid")
			break
		}

		if candidateErr := s.webrtc.AddICECandidate(sessionID, payload.Candidate); candidateErr != nil {
			s.logger.DebugContext(ctx, "webrtc candidate rejected", "session_id", sessionID, "error", candidateErr)
			response = webrtcError(message.ID, "candidate_failed", candidateErr.Error())

			break
		}

		response = Message{Type: "ack", ID: message.ID}
	case "close":
		var payload webrtcClosePayload
		if json.Unmarshal(message.Payload, &payload) != nil || payload.SessionID != sessionID {
			response = webrtcError(message.ID, "invalid_close", "close is invalid")
			break
		}

		_ = s.webrtc.Close(sessionID)
		response = Message{Type: "ack", ID: message.ID}
		sessionID = ""
	default:
		response = webrtcError(message.ID, "invalid_message", "message is invalid")
	}

	return sessionID, response
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

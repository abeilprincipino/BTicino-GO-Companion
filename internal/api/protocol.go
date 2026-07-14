package api

import (
	"encoding/json"
	"errors"
)

var ErrInvalidMessage = errors.New("invalid websocket message")

type Message struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Action  string          `json:"action,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Command struct {
	ID      string
	Action  string
	Payload json.RawMessage
}

func ParseMessage(data []byte) (Message, error) {
	var message Message
	if json.Unmarshal(data, &message) != nil {
		return Message{}, ErrInvalidMessage
	}
	switch message.Type {
	case "ping":
		if message.ID == "" || message.Action != "" || len(message.Payload) != 0 {
			return Message{}, ErrInvalidMessage
		}
	case "command":
		if message.ID == "" || message.Action == "" {
			return Message{}, ErrInvalidMessage
		}
		if len(message.Payload) == 0 {
			message.Payload = json.RawMessage("{}")
		}
		var payload map[string]any
		if json.Unmarshal(message.Payload, &payload) != nil {
			return Message{}, ErrInvalidMessage
		}
	default:
		return Message{}, ErrInvalidMessage
	}
	return message, nil
}

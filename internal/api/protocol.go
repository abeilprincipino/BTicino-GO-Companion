package api

import (
	"encoding/json"
	"errors"
)

var ErrInvalidMessage = errors.New("invalid websocket message")

type Message struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func ParseMessage(data []byte) (Message, error) {
	var message Message
	if json.Unmarshal(data, &message) != nil {
		return Message{}, ErrInvalidMessage
	}

	switch message.Type {
	case "ping":
		if message.ID == "" || len(message.Payload) != 0 {
			return Message{}, ErrInvalidMessage
		}
	default:
		return Message{}, ErrInvalidMessage
	}

	return message, nil
}

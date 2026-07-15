package openwebnet

import (
	"bytes"
	"errors"
)

var ErrInvalidDatagram = errors.New("invalid openwebnet datagram")

type Message struct {
	System string
	Raw    string
}

type Parser struct{}

func NewParser() *Parser {
	return &Parser{}
}

func (Parser) Parse(datagram []byte) (Message, error) {
	if len(datagram) < 16 {
		return Message{}, ErrInvalidDatagram
	}

	systemEnd := bytes.IndexByte(datagram[8:], 0)
	if systemEnd < 0 {
		return Message{}, ErrInvalidDatagram
	}

	systemEnd += 8
	system := string(datagram[8:systemEnd])

	systemOffset := 12
	if system == "REGISTRATION" {
		systemOffset = 16
	}

	searchStart := systemEnd + systemOffset
	if searchStart > len(datagram) {
		return Message{}, ErrInvalidDatagram
	}

	messageEnd := bytes.IndexByte(datagram[searchStart:], 0)
	if messageEnd < 0 {
		messageEnd = len(datagram)
	} else {
		messageEnd += searchStart
	}

	messageOffset := 13
	if system == "LCM_SELF_TEST" {
		messageOffset = 0
	}

	messageStart := systemEnd + messageOffset
	if messageStart > len(datagram) || messageStart > messageEnd {
		return Message{}, ErrInvalidDatagram
	}

	return Message{System: system, Raw: string(datagram[messageStart:messageEnd])}, nil
}

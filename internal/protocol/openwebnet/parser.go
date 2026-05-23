package openwebnetproto

import (
	"bytes"
	"errors"
)

type Message struct {
	System string
	Raw    string
}

var ErrInvalidDatagram = errors.New("invalid openwebnet datagram")

type Parser struct{}

func NewParser() *Parser {
	return &Parser{}
}

func (p *Parser) Parse(datagram []byte) (Message, error) {
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

	msgEnd := bytes.IndexByte(datagram[searchStart:], 0)
	if msgEnd < 0 {
		msgEnd = len(datagram)
	} else {
		msgEnd += searchStart
	}

	msgOffset := 13
	if system == "LCM_SELF_TEST" {
		msgOffset = 0
	}

	msgStart := systemEnd + msgOffset
	if msgStart > len(datagram) || msgStart > msgEnd {
		return Message{}, ErrInvalidDatagram
	}

	return Message{System: system, Raw: string(datagram[msgStart:msgEnd])}, nil
}

package openwebnet

import (
	"errors"
	"testing"
)

func TestParserParse(t *testing.T) {
	parser := NewParser()
	message, err := parser.Parse(buildPacket("OPEN", "*8*1#1#4#10*21##"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if message.System != "OPEN" || message.Raw != "*8*1#1#4#10*21##" {
		t.Fatalf("unexpected message: %+v", message)
	}
}

func TestParserRejectsInvalidDatagrams(t *testing.T) {
	parser := NewParser()
	for _, datagram := range [][]byte{
		[]byte("short"),
		append(append(make([]byte, 8), []byte("OPEN")...), 0),
	} {
		if _, err := parser.Parse(datagram); !errors.Is(err, ErrInvalidDatagram) {
			t.Fatalf("got error %v, want ErrInvalidDatagram", err)
		}
	}
}

func buildPacket(system, message string) []byte {
	payload := make([]byte, 0, 64)
	payload = append(payload, make([]byte, 8)...)
	payload = append(payload, system...)
	payload = append(payload, 0)
	payload = append(payload, []byte("ABCDEFGHIJKL")...)
	payload = append(payload, message...)
	payload = append(payload, 0)
	return payload
}

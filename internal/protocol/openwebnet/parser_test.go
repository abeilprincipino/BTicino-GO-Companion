package openwebnet

import "testing"

func TestParserParse(t *testing.T) {
	p := NewParser()
	packet := buildPacket("OPEN", "*8*1#1#4#10*21##")
	msg, err := p.Parse(packet)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if msg.System != "OPEN" {
		t.Fatalf("unexpected system: %s", msg.System)
	}
	if msg.Raw != "*8*1#1#4#10*21##" {
		t.Fatalf("unexpected raw: %s", msg.Raw)
	}
}

func TestParserParseInvalid(t *testing.T) {
	p := NewParser()
	if _, err := p.Parse([]byte("short")); err == nil {
		t.Fatal("expected parse error")
	}
}

func buildPacket(system, msg string) []byte {
	payload := make([]byte, 0, 64)
	payload = append(payload, make([]byte, 8)...)
	payload = append(payload, []byte(system)...)
	payload = append(payload, 0)
	padding := []byte("ABCDEFGHIJKL")
	payload = append(payload, padding...)
	payload = append(payload, []byte(msg)...)
	payload = append(payload, 0)
	return payload
}

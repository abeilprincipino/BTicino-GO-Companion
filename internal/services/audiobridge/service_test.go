package audiobridge

import (
	"strings"
	"testing"
)

func TestTailWriterKeepsOnlyTail(t *testing.T) {
	const cap = 8
	w := newTailWriter(cap)

	full := "hello world!!" // 13 bytes, written across two calls
	if _, err := w.Write([]byte("hello ")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if _, err := w.Write([]byte("world!!")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	got := w.String()
	if len(got) != cap {
		t.Fatalf("expected tail capped at %d bytes, got %d: %q", cap, len(got), got)
	}
	if want := full[len(full)-cap:]; got != want {
		t.Fatalf("expected tail %q, got %q", want, got)
	}
	if !strings.HasSuffix(full, got) {
		t.Fatalf("tail %q is not a suffix of the full stream %q", got, full)
	}
}

func TestTailWriterExactTailContent(t *testing.T) {
	w := newTailWriter(5)
	w.Write([]byte("abcdefghij"))
	if got := w.String(); got != "fghij" {
		t.Fatalf("expected last 5 bytes 'fghij', got %q", got)
	}

	// A single write shorter than the cap is kept verbatim.
	w2 := newTailWriter(5)
	w2.Write([]byte("ab"))
	if got := w2.String(); got != "ab" {
		t.Fatalf("expected 'ab', got %q", got)
	}

	// Cumulative writes crossing the cap keep the true tail.
	w3 := newTailWriter(4)
	w3.Write([]byte("12"))
	w3.Write([]byte("345"))
	if got := w3.String(); got != "2345" {
		t.Fatalf("expected '2345', got %q", got)
	}

	// A single write larger than the cap keeps only its own tail.
	w4 := newTailWriter(3)
	w4.Write([]byte("abcdef"))
	if got := w4.String(); got != "def" {
		t.Fatalf("expected 'def', got %q", got)
	}
}

func TestTailWriterReportsFullByteCount(t *testing.T) {
	w := newTailWriter(4)
	n, err := w.Write([]byte("abcdef"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if n != 6 {
		t.Fatalf("Write must report all bytes consumed, got %d", n)
	}
}

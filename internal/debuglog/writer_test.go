package debuglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriterWritesAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "companion.log")
	w, err := NewRotatingWriter(path, 1024)
	if err != nil {
		t.Fatalf("new writer failed: %v", err)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if _, err := w.Write([]byte("world\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(b) != "hello\nworld\n" {
		t.Fatalf("unexpected content: %q", string(b))
	}
}

func TestRotatingWriterRotatesAtMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "companion.log")
	w, err := NewRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("new writer failed: %v", err)
	}
	if _, err := w.Write([]byte("0123456789")); err != nil { // fills exactly to max
		t.Fatalf("write failed: %v", err)
	}
	if _, err := w.Write([]byte("NEW")); err != nil { // must rotate first
		t.Fatalf("write failed: %v", err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("expected rotated file: %v", err)
	}
	if string(rotated) != "0123456789" {
		t.Fatalf("unexpected rotated content: %q", string(rotated))
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current failed: %v", err)
	}
	if string(current) != "NEW" {
		t.Fatalf("unexpected current content: %q", string(current))
	}
}

func TestRotatingWriterReplacesOldBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "companion.log")
	if err := os.WriteFile(path+".1", []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed backup failed: %v", err)
	}
	w, err := NewRotatingWriter(path, 4)
	if err != nil {
		t.Fatalf("new writer failed: %v", err)
	}
	if _, err := w.Write([]byte("aaaa")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if _, err := w.Write([]byte("bb")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read rotated failed: %v", err)
	}
	if strings.Contains(string(rotated), "stale") {
		t.Fatalf("old backup not replaced: %q", string(rotated))
	}
}

package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFileRotatesAndPreservesPermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "companion.log")
	file, err := newRotatingFile(path)
	if err != nil {
		t.Fatalf("new rotating file: %v", err)
	}
	defer file.Close()

	if _, err := file.file.WriteString(strings.Repeat("a", maxSize)); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if _, err := file.Write([]byte("next\n")); err != nil {
		t.Fatalf("rotate log: %v", err)
	}

	archived, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read archived log: %v", err)
	}
	if len(archived) != maxSize {
		t.Fatalf("archive size = %d, want %d", len(archived), maxSize)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active log: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %o, want 600", info.Mode().Perm())
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	if _, err := parseLevel("info"); err != nil {
		t.Fatalf("parse info: %v", err)
	}
	if _, err := parseLevel("invalid"); err == nil {
		t.Fatal("invalid level succeeded")
	}
}

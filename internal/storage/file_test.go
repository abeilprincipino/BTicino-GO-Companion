package storage

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWritePrivateFileExclusiveWritesOnlyOneValue(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "credentials.yaml")
	values := [][]byte{[]byte("first"), []byte("second")}
	errorsByWrite := make([]error, len(values))

	var group sync.WaitGroup
	for index, value := range values {
		group.Go(func() {
			errorsByWrite[index] = WritePrivateFile(path, ".credentials-*", value, true)
		})
	}

	group.Wait()

	successes := 0

	for _, err := range errorsByWrite {
		if err == nil {
			successes++
			continue
		}

		if !errors.Is(err, os.ErrExist) {
			t.Fatalf("write error = %v, want os.ErrExist", err)
		}
	}

	if successes != 1 {
		t.Fatalf("successful writes = %d, want 1", successes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read private file: %v", err)
	}

	if string(data) != "first" && string(data) != "second" {
		t.Fatalf("private file data = %q, want one submitted value", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat private file: %v", err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private file permissions = %o, want 600", info.Mode().Perm())
	}
}

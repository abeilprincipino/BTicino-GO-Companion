package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

// WritePrivateFile atomically replaces path with data using owner-only permissions.
func WritePrivateFile(path, temporaryPattern string, data []byte, exclusive bool) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(directory, temporaryPattern)
	if err != nil {
		return err
	}

	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck // best-effort cleanup

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}

	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}

	if err := temporary.Close(); err != nil {
		return err
	}

	if exclusive {
		if err := os.Link(temporaryPath, path); err != nil {
			return fmt.Errorf("create private file exclusively: %w", err)
		}

		return syncDirectory(directory)
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace private file: %w", err)
	}

	return syncDirectory(directory)
}

func syncDirectory(directory string) error {
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open private file directory: %w", err)
	}
	defer dir.Close() //nolint:errcheck // syncing has already completed

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync private file directory: %w", err)
	}

	return nil
}

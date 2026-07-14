package homekit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type FileStateStore struct {
	path string
}

func NewFileStateStore(path string) *FileStateStore {
	return &FileStateStore{path: path}
}

func (s *FileStateStore) Load() (State, error) {
	if s == nil || s.path == "" {
		return State{}, ErrStateStoreUnavailable
	}
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Pairings: map[string]Pairing{}}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("open homekit state: %w", err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode homekit state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return State{}, fmt.Errorf("decode homekit state document: %w", err)
		}
		return State{}, errors.New("homekit state must contain one document")
	}
	if state.Pairings == nil {
		state.Pairings = map[string]Pairing{}
	}
	return state, nil
}

func (s *FileStateStore) Save(state State) error {
	if s == nil || s.path == "" {
		return ErrStateStoreUnavailable
	}
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode homekit state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create homekit state directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".homekit-state-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary homekit state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary homekit state mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary homekit state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary homekit state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary homekit state: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace homekit state: %w", err)
	}
	return nil
}

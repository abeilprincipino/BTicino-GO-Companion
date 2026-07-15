package homekit

import (
	"bticino-go-companion/internal/storage"
	"errors"
	"fmt"
	"io"
	"os"

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

	defer file.Close() //nolint:errcheck // close error not meaningful for read-only handle

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

	if err := storage.WritePrivateFile(s.path, ".homekit-state-*.yaml", data, false); err != nil {
		return fmt.Errorf("save homekit state: %w", err)
	}

	return nil
}

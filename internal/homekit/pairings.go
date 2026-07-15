package homekit

import (
	"errors"
	"sort"
	"sync"
)

var (
	ErrStateStoreUnavailable = errors.New("homekit: state store is unavailable")
	ErrInvalidPairing        = errors.New("homekit: invalid pairing")
	ErrPairingNotFound       = errors.New("homekit: pairing not found")
)

type Pairing struct {
	Identifier string `yaml:"identifier"`
	PublicKey  []byte `yaml:"public_key"`
	Admin      bool   `yaml:"admin"`
}

type State struct {
	Pairings map[string]Pairing `yaml:"pairings"`
}

type StateStore interface {
	Load() (State, error)
	Save(State) error
}

type PairingStore interface {
	Put(Pairing) error
	Get(string) (Pairing, bool, error)
	List() ([]Pairing, error)
	Delete(string) error
	Clear() error
}

type PersistentPairingStore struct {
	mu    sync.Mutex
	state StateStore
}

func NewPairingStore(state StateStore) (*PersistentPairingStore, error) {
	if state == nil {
		return nil, ErrStateStoreUnavailable
	}

	return &PersistentPairingStore{state: state}, nil
}

func (s *PersistentPairingStore) Put(pairing Pairing) error {
	if !validPairing(pairing) {
		return ErrInvalidPairing
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.state.Load()
	if err != nil {
		return err
	}

	if state.Pairings == nil {
		state.Pairings = make(map[string]Pairing)
	}

	state.Pairings[pairing.Identifier] = clonePairing(pairing)

	return s.state.Save(state)
}

func (s *PersistentPairingStore) Get(identifier string) (Pairing, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.state.Load()
	if err != nil {
		return Pairing{}, false, err
	}

	pairing, ok := state.Pairings[identifier]

	return clonePairing(pairing), ok, nil
}

func (s *PersistentPairingStore) List() ([]Pairing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.state.Load()
	if err != nil {
		return nil, err
	}

	pairings := make([]Pairing, 0, len(state.Pairings))
	for _, pairing := range state.Pairings {
		pairings = append(pairings, clonePairing(pairing))
	}

	sort.Slice(pairings, func(i, j int) bool {
		return pairings[i].Identifier < pairings[j].Identifier
	})

	return pairings, nil
}

func (s *PersistentPairingStore) Delete(identifier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.state.Load()
	if err != nil {
		return err
	}

	if _, ok := state.Pairings[identifier]; !ok {
		return ErrPairingNotFound
	}

	delete(state.Pairings, identifier)

	return s.state.Save(state)
}

func (s *PersistentPairingStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.state.Load()
	if err != nil {
		return err
	}

	state.Pairings = map[string]Pairing{}

	return s.state.Save(state)
}

func validPairing(pairing Pairing) bool {
	return pairing.Identifier != "" && len(pairing.PublicKey) == 32
}

func clonePairing(pairing Pairing) Pairing {
	pairing.PublicKey = append([]byte(nil), pairing.PublicKey...)
	return pairing
}

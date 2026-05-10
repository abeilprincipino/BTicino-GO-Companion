package runtime

import "sync"

type ComponentStatus struct {
	Enabled bool   `json:"enabled"`
	Ready   bool   `json:"ready"`
	Error   string `json:"error,omitempty"`
}

type Snapshot struct {
	SIP        ComponentStatus `json:"sip"`
	OpenWebNet ComponentStatus `json:"openwebnet"`
	Control    ComponentStatus `json:"control"`
}

type Status struct {
	mu       sync.RWMutex
	snapshot Snapshot
}

func New(sipEnabled bool, openwebnetEnabled bool) *Status {
	return &Status{
		snapshot: Snapshot{
			SIP:        ComponentStatus{Enabled: sipEnabled, Ready: !sipEnabled},
			OpenWebNet: ComponentStatus{Enabled: openwebnetEnabled, Ready: !openwebnetEnabled},
			Control:    ComponentStatus{Enabled: true, Ready: false},
		},
	}
}

func (s *Status) SetSIPReady(ready bool, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.SIP.Ready = ready
	s.snapshot.SIP.Error = err
}

func (s *Status) SetOpenWebNetReady(ready bool, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.OpenWebNet.Ready = ready
	s.snapshot.OpenWebNet.Error = err
}

func (s *Status) SetControlReady(ready bool, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.Control.Ready = ready
	s.snapshot.Control.Error = err
}

func (s *Status) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

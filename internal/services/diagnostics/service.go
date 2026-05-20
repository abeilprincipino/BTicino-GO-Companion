package diagnostics

import (
	"context"
	"log"
	"sync"
	"time"

	"bticino-go-companion/internal/system"
)

const defaultRefreshInterval = 15 * time.Second

type NetworkSnapshot struct {
	IP           string     `json:"ip,omitempty"`
	Netmask      string     `json:"netmask,omitempty"`
	MAC          string     `json:"mac,omitempty"`
	WiFiStrength *int       `json:"wifi_strength,omitempty"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
	Stale        bool       `json:"stale"`
}

type detectorFunc func() (system.NetworkSnapshot, bool)

type Service struct {
	mu       sync.RWMutex
	interval time.Duration
	logger   *log.Logger
	detect   detectorFunc
	network  NetworkSnapshot
}

func New(interval time.Duration, logger *log.Logger) *Service {
	if interval <= 0 {
		interval = defaultRefreshInterval
	}
	return &Service{
		interval: interval,
		logger:   logger,
		detect:   system.DetectNetworkSnapshot,
	}
}

func NewForTest(interval time.Duration, logger *log.Logger, detect detectorFunc) *Service {
	svc := New(interval, logger)
	if detect != nil {
		svc.detect = detect
	}
	return svc
}

func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.Refresh()

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Refresh()
		}
	}
}

func (s *Service) Refresh() {
	if s == nil || s.detect == nil {
		return
	}

	netSnap, ok := s.detect()
	if !ok {
		s.mu.Lock()
		wasStale := s.network.Stale
		s.network.Stale = true
		s.mu.Unlock()
		if s.logger != nil && !wasStale {
			s.logger.Printf("diagnostics refresh failed; keeping last known network snapshot")
		}
		return
	}

	now := time.Now()
	next := NetworkSnapshot{
		IP:        netSnap.IP,
		Netmask:   netSnap.Netmask,
		MAC:       netSnap.MAC,
		UpdatedAt: &now,
		Stale:     false,
	}
	if netSnap.WiFiRSSI != nil {
		v := *netSnap.WiFiRSSI
		next.WiFiStrength = &v
	}

	s.mu.Lock()
	wasStale := s.network.Stale
	s.network = next
	s.mu.Unlock()
	if s.logger != nil && wasStale {
		s.logger.Printf("diagnostics refresh recovered")
	}
}

func (s *Service) NetworkSnapshot() NetworkSnapshot {
	if s == nil {
		return NetworkSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.network
	if s.network.WiFiStrength != nil {
		v := *s.network.WiFiStrength
		out.WiFiStrength = &v
	}
	if s.network.UpdatedAt != nil {
		ts := *s.network.UpdatedAt
		out.UpdatedAt = &ts
	}
	return out
}

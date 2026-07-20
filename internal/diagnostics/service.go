// Package diagnostics collects cached OpenWebNet and local device diagnostics.
package diagnostics

import (
	"bticino-go-companion/internal/openwebnet"
	"bticino-go-companion/internal/system"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const refreshInterval = 15 * time.Minute

type Reader interface {
	DiagnosticSnapshot(context.Context) (openwebnet.DiagnosticSnapshot, error)
}

type Local struct {
	Model        string `json:"model,omitempty"`
	Interface    string `json:"interface,omitempty"`
	WiFiStrength *int   `json:"wifi_strength,omitempty"`
}

type Snapshot struct {
	OpenWebNet   openwebnet.DiagnosticSnapshot `json:"openwebnet"`
	Local        Local                         `json:"local"`
	RefreshedAt  time.Time                     `json:"refreshed_at,omitempty"`
	RefreshError string                        `json:"refresh_error,omitempty"`
}

type Service struct {
	reader   Reader
	model    string
	onChange func()
	logger   *slog.Logger

	mu       sync.RWMutex
	snapshot Snapshot
}

func New(reader Reader, model string, onChange func()) *Service {
	return &Service{reader: reader, model: strings.TrimSpace(model), onChange: onChange, logger: slog.Default().With("component", "diagnostics")}
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

// Start refreshes in the background so diagnostics never delay readiness.
func (s *Service) Start(ctx context.Context) {
	go func() {
		s.Refresh(ctx)
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Refresh(ctx)
			}
		}
	}()
}

func (s *Service) Refresh(ctx context.Context) {
	local := detectLocal(s.model)
	var value openwebnet.DiagnosticSnapshot
	var err error
	if s.reader == nil {
		err = fmt.Errorf("diagnostics reader is unavailable")
	} else {
		value, err = s.reader.DiagnosticSnapshot(ctx)
	}
	s.mu.Lock()
	hadError := s.snapshot.RefreshError != ""
	if err == nil {
		s.snapshot.OpenWebNet = value
	}
	s.snapshot.Local = local
	s.snapshot.RefreshedAt = time.Now().UTC()
	if err != nil {
		s.snapshot.RefreshError = err.Error()
	} else {
		s.snapshot.RefreshError = ""
	}
	s.mu.Unlock()
	if err != nil {
		s.logger.WarnContext(ctx, "diagnostics refresh failed", "error", err)
	} else if hadError {
		s.logger.InfoContext(ctx, "diagnostics refresh recovered")
	} else {
		s.logger.DebugContext(ctx, "diagnostics refresh completed")
	}
	if s.onChange != nil {
		s.onChange()
	}
}

func detectLocal(model string) Local {
	local := Local{Model: model}
	network, ok := system.DetectNetworkSnapshot()
	if !ok {
		return local
	}
	local.Interface = network.Interface
	local.WiFiStrength = network.WiFiStrength
	return local
}

package systemcontrol

import (
	"context"
	"errors"
	"strings"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/system"
)

var (
	ErrSystemControlUnavailable = errors.New("system control unavailable")
	ErrRebootDisabled           = errors.New("reboot control disabled")
	ErrServiceDisabled          = errors.New("service is disabled")
	ErrServiceNotExposed        = errors.New("service is not exposed")
	ErrServiceNameInvalid       = errors.New("service name is invalid")
)

type Service struct {
	manager        system.ServiceManager
	rebootEnabled  bool
	serviceConfigs map[string]config.SystemServiceConfig
}

func New(
	manager system.ServiceManager,
	rebootEnabled bool,
	serviceConfigs map[string]config.SystemServiceConfig,
) *Service {
	configs := make(map[string]config.SystemServiceConfig, len(serviceConfigs))
	for rawName, cfg := range serviceConfigs {
		name := normalizeName(rawName)
		if name == "" {
			continue
		}
		configs[name] = config.SystemServiceConfig{
			Enabled: cfg.Enabled,
			Exposed: cfg.Exposed,
		}
	}
	return &Service{
		manager:        manager,
		rebootEnabled:  rebootEnabled,
		serviceConfigs: configs,
	}
}

func (s *Service) RebootEnabled() bool {
	return s != nil && s.rebootEnabled
}

func (s *Service) ServiceConfigs() map[string]config.SystemServiceConfig {
	if s == nil || len(s.serviceConfigs) == 0 {
		return nil
	}
	out := make(map[string]config.SystemServiceConfig, len(s.serviceConfigs))
	for name, cfg := range s.serviceConfigs {
		out[name] = cfg
	}
	return out
}

func (s *Service) Reboot(ctx context.Context) error {
	if s == nil || s.manager == nil {
		return ErrSystemControlUnavailable
	}
	if !s.rebootEnabled {
		return ErrRebootDisabled
	}
	return s.manager.RebootHost(ctx)
}

func (s *Service) RestartService(ctx context.Context, name string) error {
	serviceName, cfg, err := s.serviceConfig(name)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return ErrServiceDisabled
	}
	return s.manager.Restart(ctx, serviceName)
}

func (s *Service) ServiceStatus(ctx context.Context, name string) (system.ServiceStatus, error) {
	serviceName, cfg, err := s.serviceConfig(name)
	if err != nil {
		return system.ServiceStatus{}, err
	}
	if !cfg.Enabled {
		return system.ServiceStatus{}, ErrServiceDisabled
	}
	return s.manager.Status(ctx, serviceName)
}

func (s *Service) serviceConfig(name string) (string, config.SystemServiceConfig, error) {
	if s == nil || s.manager == nil {
		return "", config.SystemServiceConfig{}, ErrSystemControlUnavailable
	}
	serviceName := normalizeName(name)
	if serviceName == "" {
		return "", config.SystemServiceConfig{}, ErrServiceNameInvalid
	}
	cfg, ok := s.serviceConfigs[serviceName]
	if !ok {
		return "", config.SystemServiceConfig{}, ErrServiceNotExposed
	}
	return serviceName, cfg, nil
}

func normalizeName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

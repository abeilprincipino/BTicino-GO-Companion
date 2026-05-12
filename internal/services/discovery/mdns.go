package discovery

import (
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"bticino-go-companion/internal/config"
	"github.com/grandcat/zeroconf"
)

const (
	defaultMDNSServiceType = "_bticomp._tcp"
	defaultPublishInterval = 15 * time.Second
)

type advertisementState struct {
	instanceName string
	needsClaim   bool
	deviceID     string
	deviceName   string
}

// Start publishes the companion service over mDNS and refreshes TXT metadata
// when claim state changes. It keeps retrying registration failures until ctx ends.
func Start(
	ctx context.Context,
	cfg config.Config,
	needsClaimFn func() bool,
	deviceIDFn func() string,
	logger *log.Logger,
) error {
	logf := func(format string, args ...any) {
		if logger != nil {
			logger.Printf(format, args...)
		}
	}

	if !cfg.MDNSEnabled {
		logf("mdns disabled by config")
		return nil
	}

	port := parsePort(cfg.ListenAddr)
	service := normalizeServiceType(cfg.MDNSServiceType)
	baseName := normalizeName(cfg.DeviceName)
	if baseName == "" {
		baseName = "bticino-companion"
	}

	var (
		server  *zeroconf.Server
		current advertisementState
		backoff = time.Second
	)

	ticker := time.NewTicker(defaultPublishInterval)
	defer ticker.Stop()

	for {
		if server == nil {
			latest := snapshot(baseName, needsClaimFn, deviceIDFn)
			nextServer, err := register(service, port, cfg, latest)
			if err != nil {
				logf("mdns register failed service=%s port=%d err=%v", service, port, err)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
				}
				continue
			}

			server = nextServer
			current = latest
			backoff = time.Second
			logf("mdns registered service=%s port=%d needs_claim=%t instance=%s", service, port, current.needsClaim, current.instanceName)
		}

		select {
		case <-ctx.Done():
			if server != nil {
				server.Shutdown()
			}
			return nil
		case <-ticker.C:
			latest := snapshot(baseName, needsClaimFn, deviceIDFn)
			if latest == current {
				continue
			}
			if server != nil {
				server.Shutdown()
				server = nil
			}
			logf("mdns state change detected needs_claim=%t instance=%s", latest.needsClaim, latest.instanceName)
		}
	}
}

func register(service string, port int, cfg config.Config, state advertisementState) (*zeroconf.Server, error) {
	txt := []string{
		"api=v2",
		"model=" + normalizeTXT(cfg.DeviceModel),
		"fw=" + normalizeTXT(cfg.DeviceFirmware),
		"name=" + normalizeTXT(state.deviceName),
		"device_id=" + normalizeTXT(state.deviceID),
		"needs_claim=" + strconv.FormatBool(state.needsClaim),
	}
	return zeroconf.Register(state.instanceName, service, "local.", port, txt, nil)
}

func snapshot(baseName string, needsClaimFn func() bool, deviceIDFn func() string) advertisementState {
	state := advertisementState{
		needsClaim: false,
		deviceName: baseName,
	}
	if needsClaimFn != nil {
		state.needsClaim = needsClaimFn()
	}
	if deviceIDFn != nil {
		state.deviceID = normalizeTXT(deviceIDFn())
	}
	state.instanceName = baseName
	if state.deviceID != "" {
		state.instanceName = state.deviceID
	}
	return state
}

func normalizeServiceType(raw string) string {
	service := strings.TrimSpace(raw)
	if service == "" {
		return defaultMDNSServiceType
	}
	if strings.HasSuffix(service, ".") {
		service = strings.TrimSuffix(service, ".")
	}
	if !strings.HasPrefix(service, "_") {
		service = "_" + service
	}
	if !strings.HasSuffix(service, "._tcp") && !strings.HasSuffix(service, "._udp") {
		service += "._tcp"
	}
	return service
}

func parsePort(listenAddr string) int {
	trimmed := strings.TrimSpace(listenAddr)
	if trimmed == "" {
		return 8080
	}
	if strings.HasPrefix(trimmed, ":") {
		if port, ok := parseValidPort(strings.TrimPrefix(trimmed, ":")); ok {
			return port
		}
		return 8080
	}
	host, portRaw, err := net.SplitHostPort(trimmed)
	if err == nil {
		_ = host
		if port, ok := parseValidPort(portRaw); ok {
			return port
		}
		return 8080
	}
	if port, ok := parseValidPort(trimmed); ok {
		return port
	}
	return 8080
}

func parseValidPort(raw string) (int, bool) {
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func normalizeName(raw string) string {
	name := normalizeTXT(raw)
	if name == "" {
		return ""
	}
	return name
}

func normalizeTXT(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	return strings.ReplaceAll(trimmed, " ", "_")
}

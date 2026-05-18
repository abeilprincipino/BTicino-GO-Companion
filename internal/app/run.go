package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bticino-go-companion/internal/adapters/http/v2"
	"bticino-go-companion/internal/adapters/openwebnet"
	"bticino-go-companion/internal/adapters/rtsp"
	"bticino-go-companion/internal/adapters/sip"
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/event"
	"bticino-go-companion/internal/protocol/openwebnet"
	"bticino-go-companion/internal/security"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/discovery"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/media"
	"bticino-go-companion/internal/services/runtime"
	"bticino-go-companion/internal/services/state"
	"bticino-go-companion/internal/services/systemcontrol"
	"bticino-go-companion/internal/services/trace"
	"bticino-go-companion/internal/system"
)

func Run(ctx context.Context, cfgPath string, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}

	resolvedConfigPath, err := config.ResolvePath(cfgPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	cfg, created, err := loadOrCreateConfig(resolvedConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if created {
		logger.Printf("created default config at %s", resolvedConfigPath)
	}

	commandClient := openwebnet.NewCommandClient(cfg)
	if cfg.OpenWebNetEnabled {
		if err := enrichConfigWithDiagnosticMetadataWithRetry(resolvedConfigPath, &cfg, commandClient, logger); err != nil {
			logger.Printf("device diagnostics bootstrap skipped: %v", err)
		}
	}

	authStore, err := auth.NewStore(resolvedConfigPath, cfg.ClaimCode, cfg.DeviceModel, cfg.DeviceMAC)
	if err != nil {
		return fmt.Errorf("init auth store: %w", err)
	}

	go func() {
		if err := discovery.Start(ctx, cfg, authStore.NeedsClaim, authStore.DeviceID, logger); err != nil {
			logger.Printf("mdns service stopped: %v", err)
		}
	}()

	guard := security.NewGuard()

	projector := state.NewProjector(cfg.Entrypoints)
	eventBroker := events.New(512)
	traceBroker := trace.New(1024)
	normalizer := events.NewNormalizer(cfg.Entrypoints)
	validator := events.NewValidator()
	runtimeStatus := runtime.New(cfg.MediaSIPEnabled, cfg.OpenWebNetEnabled)

	publish := func(ev event.Envelope) {
		normalized := normalizer.Normalize(ev)
		if err := validator.Validate(normalized); err != nil {
			logger.Printf("event validation warning: %v type=%s source=%s", err, normalized.Type, normalized.Source)
			normalized = event.Envelope{
				Type:   event.TypeEventInvalid,
				TS:     normalized.TS,
				Source: event.SourceSystem,
				Payload: map[string]any{
					"error": err.Error(),
					"original": map[string]any{
						"type":          normalized.Type,
						"source":        normalized.Source,
						"entrypoint_id": normalized.EntrypointID,
						"raw":           normalized.Raw,
					},
				},
				Raw: normalized.Raw,
			}
		}
		enriched := projector.Apply(normalized)
		eventBroker.Publish(enriched)
	}

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				publish(event.Envelope{
					Type:   event.TypeHeartbeat,
					TS:     time.Now(),
					Source: event.SourceSystem,
				})
			}
		}
	}()

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      nil,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  30 * time.Second,
	}

	sipManager := sipadapter.NewManager(cfg, logger)
	sipManager.SetEventSink(func(ev event.Envelope) {
		publish(ev)
	})
	if err := sipManager.Start(ctx); err != nil {
		runtimeStatus.SetSIPReady(false, err.Error())
		return fmt.Errorf("start sip manager: %w", err)
	}
	runtimeStatus.SetSIPReady(true, "")
	defer func() {
		if err := sipManager.Close(); err != nil {
			logger.Printf("sip manager close warning: %v", err)
		}
	}()

	commandClient.SetTraceSink(func(direction string, payload map[string]any) {
		rec := trace.Record{
			Direction: direction,
			Transport: "tcp_command",
		}
		if frame, ok := payload["frame"].(string); ok {
			rec.Frame = frame
		}
		traceBroker.Publish(rec)
	})
	if cfg.OpenWebNetEnabled {
		go func() {
			bootCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			muted, err := commandClient.AudioMutedStatus(bootCtx)
			if err != nil {
				if errors.Is(err, openwebnet.ErrStatusUnavailable) {
					logger.Printf("audio status bootstrap unavailable; waiting for runtime events")
				} else {
					logger.Printf("audio status bootstrap skipped: %v", err)
				}
				return
			}
			kind := event.TypeAudioUnmuted
			if muted {
				kind = event.TypeAudioMuted
			}
			publish(event.Envelope{
				Type:   kind,
				TS:     time.Now(),
				Source: event.SourceSystem,
				Payload: map[string]any{
					"source": "bootstrap_probe",
				},
			})
		}()
		if !strings.EqualFold(strings.TrimSpace(cfg.DeviceModel), "C100X") {
			go func() {
				bootCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				voicemailStatus, err := commandClient.VoicemailStatus(bootCtx)
				if err != nil {
					if errors.Is(err, openwebnet.ErrStatusUnavailable) {
						logger.Printf("voicemail status bootstrap unavailable; waiting for runtime events")
					} else {
						logger.Printf("voicemail status bootstrap skipped: %v", err)
					}
					return
				}
				kind := event.TypeVoicemailDisabled
				if voicemailStatus.Enabled {
					kind = event.TypeVoicemailEnabled
				}
				publish(event.Envelope{
					Type:   kind,
					TS:     time.Now(),
					Source: event.SourceSystem,
					Payload: map[string]any{
						"source":                  "bootstrap_probe",
						"welcome_message_enabled": voicemailStatus.WelcomeMessageEnabled,
					},
				})
			}()
		}
	}
	mediaBackend := media.NewCompositeBackend(
		sipManager,
		commandClient,
		cfg.MediaRTPAudioPort,
		cfg.MediaRTPVideoPort,
	)
	mediaService := media.NewService(mediaBackend)
	mediaService.SetTransitionSink(func(tr media.Transition) {
		if strings.TrimSpace(tr.Kind) == "" {
			return
		}
		payload := map[string]any{
			"source": strings.TrimSpace(tr.Source),
			"reason": strings.TrimSpace(tr.Reason),
		}
		if strings.TrimSpace(tr.DevAddr) != "" {
			payload["devaddr"] = strings.TrimSpace(tr.DevAddr)
		}
		publish(event.Envelope{
			Type:         tr.Kind,
			TS:           time.Now(),
			Source:       strings.TrimSpace(tr.Source),
			EntrypointID: strings.TrimSpace(tr.EntrypointID),
			Payload:      payload,
		})
	})

	if cfg.MediaRTSPEnabled {
		rtspServer := rtspadapter.NewServer(cfg, logger, mediaService)
		if err := rtspServer.Start(ctx); err != nil {
			return fmt.Errorf("start rtsp server: %w", err)
		}
	}

	var audioClient *openwebnet.CommandClient
	if cfg.MuteEnabled {
		audioClient = commandClient
	}
	voicemailClient := commandClient
	if !cfg.VoicemailEnabled || strings.EqualFold(strings.TrimSpace(cfg.DeviceModel), "C100X") {
		voicemailClient = nil
	}
	controlService := control.New(cfg.Entrypoints, mediaService, commandClient, sipManager, audioClient, voicemailClient, func(ev event.Envelope) {
		publish(ev)
	})
	runtimeStatus.SetControlReady(true, "")

	systemControl := systemcontrol.New(
		system.NewInitServiceManager(),
		cfg.SystemRebootEnabled,
		cfg.SystemServices,
	)

	router := v2.NewRouter(cfg, authStore, guard, projector, controlService, eventBroker, runtimeStatus, traceBroker, systemControl)
	srv.Handler = router.Handler()

	if cfg.OpenWebNetEnabled {
		listener := openwebnet.NewListener(cfg.OpenWebNetGroup, cfg.OpenWebNetListenPort, cfg.OpenWebNetReadBuffer)
		listener.SetTraceSink(func(msg openwebnetproto.Message, mapped []event.Envelope) {
			rec := trace.Record{
				Direction: "rx",
				Transport: "udp_multicast",
				System:    msg.System,
				Frame:     msg.Raw,
				Mapped:    len(mapped) > 0,
			}
			if len(mapped) > 0 {
				rec.DecodedEventType = make([]string, 0, len(mapped))
				for _, mappedEvent := range mapped {
					rec.DecodedEventType = append(rec.DecodedEventType, mappedEvent.Type)
				}
			}
			traceBroker.Publish(rec)
		})
		runtimeStatus.SetOpenWebNetReady(true, "")
		go func() {
			if err := listener.Run(ctx, func(ev event.Envelope) { publish(ev) }); err != nil {
				runtimeStatus.SetOpenWebNetReady(false, err.Error())
				logger.Printf("openwebnet listener stopped: %v", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Printf("companion v2 api listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server failed: %w", err)
	}
	return nil
}

func loadOrCreateConfig(path string) (config.Config, bool, error) {
	dir := filepath.Dir(path)
	_, err := os.Stat(path)
	if err == nil {
		cfg, loadErr := config.Load(path)
		if loadErr != nil {
			return config.Config{}, false, loadErr
		}
		meta := system.DetectLocalMetadata()
		changed := false

		if cfg.DataDir != dir {
			cfg.DataDir = dir
			changed = true
		}
		if strings.TrimSpace(cfg.ClaimCode) == "" {
			cfg.ClaimCode = defaultClaimCode()
			changed = true
		}
		model := normalizedDetectedModel(cfg.DeviceModel)
		if model == "" {
			model = normalizedDetectedModel(meta.Model)
			if model == "" {
				return config.Config{}, false, fmt.Errorf("device model detection failed: model is unknown in config and runtime metadata")
			}
			cfg.DeviceModel = model
			changed = true
		}
		firmware := strings.TrimSpace(cfg.DeviceFirmware)
		if firmware == "" || strings.EqualFold(firmware, "unknown") {
			firmware = strings.TrimSpace(meta.Firmware)
			if firmware != "" {
				cfg.DeviceFirmware = firmware
			}
		}
		if strings.TrimSpace(cfg.DeviceIP) == "" {
			ip := strings.TrimSpace(meta.Network.IP)
			if ip != "" {
				cfg.DeviceIP = ip
			}
		}
		if strings.TrimSpace(cfg.DeviceMAC) == "" || cfg.DeviceMAC == "00:00:00:00:00:00" {
			mac := strings.TrimSpace(meta.Network.MAC)
			if mac == "" {
				mac = strings.TrimSpace(system.DetectDeviceMAC())
			}
			if mac == "" {
				mac = "00:00:00:00:00:00"
			}
			cfg.DeviceMAC = mac
		}
		if cfg.DeviceWiFiRSSI == nil && meta.Network.WiFiRSSI != nil {
			rssi := *meta.Network.WiFiRSSI
			cfg.DeviceWiFiRSSI = &rssi
		}
		if changed {
			if saveErr := config.Save(path, cfg); saveErr != nil {
				return config.Config{}, false, saveErr
			}
		}
		return cfg, false, nil
	}
	if !os.IsNotExist(err) {
		return config.Config{}, false, err
	}

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ClaimCode = defaultClaimCode()
	meta := system.DetectLocalMetadata()

	model := normalizedDetectedModel(meta.Model)
	if model == "" {
		return config.Config{}, false, fmt.Errorf("device model detection failed: runtime metadata returned unknown model")
	}
	cfg.DeviceModel = model

	firmware := strings.TrimSpace(meta.Firmware)
	if firmware != "" {
		cfg.DeviceFirmware = firmware
	}

	ip := strings.TrimSpace(meta.Network.IP)
	if ip != "" {
		cfg.DeviceIP = ip
	}

	mac := strings.TrimSpace(meta.Network.MAC)
	if mac == "" {
		mac = strings.TrimSpace(system.DetectDeviceMAC())
	}
	if mac == "" {
		mac = "00:00:00:00:00:00"
	}
	cfg.DeviceMAC = mac

	if meta.Network.WiFiRSSI != nil {
		rssi := *meta.Network.WiFiRSSI
		cfg.DeviceWiFiRSSI = &rssi
	}

	if err := config.Save(path, cfg); err != nil {
		return config.Config{}, false, err
	}
	loaded, err := config.Load(path)
	if err != nil {
		return config.Config{}, false, err
	}
	loaded.DataDir = dir
	return loaded, true, nil
}

func defaultClaimCode() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	hexVal := strings.ToLower(hex.EncodeToString(buf))
	return hexVal[:4] + "-" + hexVal[4:]
}

func normalizedDetectedModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" || strings.EqualFold(trimmed, "unknown") {
		return ""
	}
	return trimmed
}

func enrichConfigWithDiagnosticMetadata(path string, cfg *config.Config, commandClient *openwebnet.CommandClient, logger *log.Logger) error {
	_ = path
	if cfg == nil || commandClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	diagnostic, err := commandClient.DiagnosticSnapshot(ctx)
	if err != nil {
		return err
	}

	setIfNonEmpty(&cfg.DeviceIP, diagnostic.IP)
	setIfNonEmpty(&cfg.DeviceNetmask, diagnostic.Netmask)
	setIfNonEmpty(&cfg.DeviceMAC, diagnostic.MAC)
	setIfNonEmpty(&cfg.DeviceFirmware, diagnostic.Firmware)
	setIfNonEmpty(&cfg.DeviceHardware, diagnostic.Hardware)
	setIfNonEmpty(&cfg.DeviceKernel, diagnostic.Kernel)
	setIfNonEmpty(&cfg.DeviceDistribution, diagnostic.Distribution)
	logger.Printf("refreshed runtime diagnostics snapshot")
	return nil
}

func enrichConfigWithDiagnosticMetadataWithRetry(path string, cfg *config.Config, commandClient *openwebnet.CommandClient, logger *log.Logger) error {
	const (
		maxAttempts = 5
		retryDelay  = 2 * time.Second
	)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := enrichConfigWithDiagnosticMetadata(path, cfg, commandClient, logger); err != nil {
			lastErr = err
			if logger != nil {
				logger.Printf("device diagnostics attempt %d/%d failed: %v", attempt, maxAttempts, err)
			}
			if attempt < maxAttempts {
				time.Sleep(retryDelay)
			}
			continue
		}
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("diagnostic bootstrap failed with unknown error")
}

func setIfNonEmpty(dst *string, value string) bool {
	if dst == nil {
		return false
	}
	next := strings.TrimSpace(value)
	if next == "" || strings.EqualFold(next, "unknown") {
		return false
	}
	if strings.TrimSpace(*dst) == next {
		return false
	}
	*dst = next
	return true
}

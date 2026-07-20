package app

import (
	"bticino-go-companion/internal/api"
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/diagnostics"
	"bticino-go-companion/internal/discovery"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/openwebnet"
	"bticino-go-companion/internal/signaling"
	"bticino-go-companion/internal/system"
	"bticino-go-companion/internal/webui"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

func Run(ctx context.Context, configPath string, logger *slog.Logger, setLogLevel func(string) error) error {
	return run(ctx, configPath, logger, setLogLevel, system.DetectMetadata, ":8080", ":80")
}

func run(
	ctx context.Context,
	configPath string,
	logger *slog.Logger,
	setLogLevel func(string) error,
	detectMetadata func() (config.Metadata, error),
	apiAddr string,
	webUIAddr string,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	appLogger := logger.With("component", "app")
	if configPath == "" {
		configPath = config.DefaultPath
	}

	configStore, created, err := openConfig(configPath, detectMetadata)
	if err != nil {
		return err
	}

	if created {
		appLogger.Info("configuration created", "path", configPath)
	}
	appLogger.Info("application starting", "config_path", configPath)
	if setLogLevel != nil {
		if err := setLogLevel(configStore.Snapshot().Logging.Level); err != nil {
			return fmt.Errorf("set log level: %w", err)
		}
	}

	projector := core.NewProjector()
	openWebNetTrace := openwebnet.NewTrace(0)
	openWebNetControl := openwebnet.NewControl(configStore.Snapshot().Companion.Entrypoints, openWebNetTrace)
	initialConfig := configStore.Snapshot()
	snapshots := media.NewSnapshotManager(system.CompanionDataDir, logger)
	if len(initialConfig.Companion.Entrypoints) == 0 {
		return errors.New("create sip runtime: no entrypoints configured")
	}
	mediaConfig, err := media.ResolveSourceConfig(initialConfig.Companion.Model, initialConfig.Companion.Entrypoints[0])
	if err != nil {
		return fmt.Errorf("resolve sip runtime source: %w", err)
	}
	dialer, err := signaling.NewStreamDialer(signaling.StreamDialerConfig{Target: mediaConfig.Target, Domain: signaling.DiscoverFlexisipDomain()})
	if err != nil {
		return fmt.Errorf("create sip runtime: %w", err)
	}
	defer func() {
		if err := dialer.Close(); err != nil {
			appLogger.Warn("sip runtime close failed", "error", err)
		}
	}()
	rtspServer, err := media.NewRTSPServer(logger, media.DefaultRTSPAddress, initialConfig.Companion.Entrypoints, func(entrypoint config.Entrypoint, events media.SourceEvents) (media.ManagedSource, func(), error) {
		return newBridgeSource(configStore.Snapshot(), logger, dialer, entrypoint, events, snapshots)
	})
	if err != nil {
		return fmt.Errorf("create rtsp server: %w", err)
	}
	if err := rtspServer.Start(ctx); err != nil {
		return err
	}
	defer func() {
		if err := rtspServer.Close(); err != nil {
			appLogger.Warn("RTSP server close failed", "error", err)
		}
	}()

	allowedServices := []string{}

	for name, svc := range configStore.Snapshot().System.Services {
		if svc.Enabled {
			allowedServices = append(allowedServices, name)
		}
	}

	rt := system.NewRuntimeControl(system.NewInitServiceAdapter(nil), system.NewRebootAdapter(nil), allowedServices)

	build := system.CurrentBuildInfo()
	updatePolicy := func() system.UpdatePolicy {
		cfg := configStore.Snapshot().System
		return system.UpdatePolicy{Enabled: cfg.UpdateEnabled, Exposed: cfg.UpdateExposed, DataDir: system.CompanionDataDir}
	}
	var updateSource system.ReleaseSource
	if strings.TrimSpace(build.ReleaseRepo) != "" {
		updateSource = system.NewGitHubReleaseClient(&http.Client{Timeout: 30 * time.Second}, "https://api.github.com/repos/"+build.ReleaseRepo+"/releases/latest")
	}
	restartCompanion := func(ctx context.Context) error { return rt.Restart(ctx, "companion") }
	updater := system.NewUpdater(updateSource, build, updatePolicy, restartCompanion)

	webrtc, err := media.NewWebRTCService(rtspServer.Coordinator(), initialConfig.Companion.Entrypoints)
	if err != nil {
		return fmt.Errorf("create WebRTC service: %w", err)
	}
	defer func() {
		if err := webrtc.Shutdown(); err != nil {
			appLogger.Warn("webrtc service shutdown failed", "error", err)
		}
	}()

	authStore := auth.NewStore(configStore)
	authStore.SetLogger(logger.With("component", "auth"))
	mdns := discovery.NewService(nil)

	server := api.NewServer(authStore, configStore, projector, logger)
	server.SetEntrypoints(openWebNetControl)
	server.SetAudio(openWebNetControl)
	server.SetVoicemail(openWebNetControl)
	server.SetWebRTC(webrtc)
	server.SetSnapshot(snapshots)
	snapshots.SetOnCaptured(server.BroadcastState)
	server.SetRuntime(rt)
	server.SetUpdate(updater)
	diagnosticService := diagnostics.New(openWebNetControl, configStore.Snapshot().Companion.Model, server.BroadcastState)
	server.SetDiagnostics(diagnosticService)

	apiListener, err := net.Listen("tcp", apiAddr)
	if err != nil {
		return fmt.Errorf("listen api: %w", err)
	}

	webUIListener, err := net.Listen("tcp", webUIAddr)
	if err != nil {
		_ = apiListener.Close()
		return fmt.Errorf("listen webui: %w", err)
	}

	apiServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	apiServer.RegisterOnShutdown(server.CloseWebSockets)
	webUI := webui.New(configStore, authStore, logger, restartCompanion, rt.Reboot, setLogLevel)
	webUI.SetFrames(openWebNetTrace)
	webUI.SetDiagnostics(diagnosticService)
	webUI.SetUpdate(updater)
	webUIServer := &http.Server{
		Handler:           webUI.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go checkUpdates(ctx, updater, server.BroadcastState, logger)

	applyEvent := func(event core.Event) {
		if _, err := projector.Apply(event); err != nil && !errors.Is(err, core.ErrInvalidTransition) {
			logger.Warn("openwebnet event apply failed", "event_type", event.Type(), "error", err)
			return
		}
		server.BroadcastState()
		server.BroadcastEvent(map[string]any{"type": event.Type()})
	}
	rtspServer.Coordinator().SetStateObserver(func(snapshot media.StreamSnapshot) {
		streamID := core.StreamID(fmt.Sprintf("media-%d", snapshot.LeaseID))
		switch {
		case snapshot.Owner == media.StreamOwnerCompanion:
			applyEvent(core.PreviewStarted{StreamID: streamID, EntrypointID: core.EntrypointID(snapshot.EntrypointID)})
		case snapshot.Owner == media.StreamOwnerIdle:
			preview := projector.Snapshot().PreviewStream
			if preview != nil && strings.HasPrefix(string(preview.StreamID), "media-") {
				applyEvent(core.PreviewStopped{StreamID: preview.StreamID})
			}
		}
	})
	listener := openwebnet.NewListener(configStore.Snapshot().Companion.Entrypoints, logger, openWebNetTrace)
	listener.SetFrameObserver(func(frame string) {
		switch {
		case openwebnet.IsStreamStartVideo(frame):
			rtspServer.ObserveControlTrack(true)
		case openwebnet.IsStreamStartAudio(frame):
			rtspServer.ObserveControlTrack(false)
		case openwebnet.IsStreamStop(frame), openwebnet.IsFreeAVResources(frame):
			rtspServer.ObserveControlStop()
		}
	})
	go func() {
		if err := listener.Run(ctx, applyEvent); err != nil && ctx.Err() == nil {
			logger.Error("openwebnet listener stopped", "component", "openwebnet.listener", "error", err)
		}
	}()
	go func() {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		events, err := openWebNetControl.InitialEvents(probeCtx)
		if err != nil {
			logger.Debug("openwebnet initial state incomplete", "error", err)
		}
		for _, event := range events {
			applyEvent(event)
		}
	}()
	diagnosticService.Start(ctx)
	go func() {
		err := mdns.Run(ctx, func() (discovery.Advertisement, error) {
			cfg := configStore.Snapshot()
			iface, err := system.PreferredOutboundInterface()
			if err != nil {
				return discovery.Advertisement{}, err
			}
			return discovery.Advertisement{
				DeviceID:     cfg.Companion.DeviceID,
				Model:        cfg.Companion.Model,
				PairingState: authStore.PairingState(),
				InstanceID:   cfg.Auth.InstanceID,
				Port:         8080,
				Interfaces:   []net.Interface{iface},
			}, nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Error("mDNS service stopped", "component", "discovery.mdns", "error", err)
		}
	}()
	return serve(ctx, appLogger, apiListener, apiServer, webUIListener, webUIServer)
}

func newBridgeSource(cfg config.Config, logger *slog.Logger, dialer signaling.StreamDialer, entrypoint config.Entrypoint, events media.SourceEvents, snapshots *media.SnapshotManager) (media.ManagedSource, func(), error) {
	backchannel, err := media.NewUDPBackchannel("")
	if err != nil {
		return nil, nil, fmt.Errorf("create udp backchannel: %w", err)
	}

	attempt := snapshots.Arm(entrypoint.ID)
	var bridge *media.AudioBridge
	source, closeSource, err := newSource(cfg, logger, dialer, entrypoint, func(packet *rtp.Packet) {
		if attempt != nil {
			attempt.Consume(packet)
		}
		if events.VideoRTP != nil {
			events.VideoRTP(packet)
		}
	}, func(packet *rtp.Packet) {
		if err := bridge.WriteIntercomSpeex(packet); err != nil {
			logger.Warn("intercom audio bridge write failed", "error", err)
		}
	}, events.RemoteBYE)
	if err != nil {
		if attempt != nil {
			attempt.Close()
		}
		_ = backchannel.Close()
		return nil, nil, err
	}
	bridge = media.NewAudioBridge(media.NewGStreamerAudioBridge(filepath.Join(system.CompanionDataDir, "gst"), logger), events.AudioRTP, backchannel, logger, events.Failed)
	return &bridgeSource{source: source, bridge: bridge}, func() {
		if attempt != nil {
			attempt.Close()
		}
		closeSource()
		if err := backchannel.Close(); err != nil {
			logger.Warn("close udp backchannel", "error", err)
		}
	}, nil
}

type bridgeSource struct {
	source *media.SourceSession
	bridge *media.AudioBridge
}

var _ media.ManagedSourceBackchannel = (*bridgeSource)(nil)

func (s *bridgeSource) Start(ctx context.Context) error {
	if err := s.bridge.Start(ctx); err != nil {
		return fmt.Errorf("start audio bridge: %w", err)
	}
	if err := s.source.Start(ctx); err != nil {
		_ = s.bridge.StopContext(ctx)
		return err
	}
	return nil
}

func (s *bridgeSource) Close(ctx context.Context) error {
	err := s.source.Close(ctx)
	if bridgeErr := s.bridge.StopContext(ctx); bridgeErr != nil && err == nil {
		err = bridgeErr
	}
	return err
}

func (s *bridgeSource) WriteBackchannelRTP(packet *rtp.Packet) error {
	return s.bridge.WriteBackchannelOpus(packet)
}

func newSource(cfg config.Config, logger *slog.Logger, dialer signaling.StreamDialer, entrypoint config.Entrypoint, videoPacket, audioPacket func(*rtp.Packet), remoteBYE func()) (*media.SourceSession, func(), error) {
	sourceConfig, err := media.ResolveSourceConfig(cfg.Companion.Model, entrypoint)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve media source: %w", err)
	}
	logger.Debug("media source configuration resolved",
		"component", "media.source",
		"model", sourceConfig.Model,
		"entrypoint_id", entrypoint.ID,
		"dev_addr", sourceConfig.DevAddr,
		"high_res_video", sourceConfig.HighResVideo,
		"target", sourceConfig.Target,
	)

	if dialer == nil {
		return nil, nil, errors.New("sip runtime is unavailable")
	}
	var source *media.SourceSession
	var sourceLive atomic.Bool
	if callbackSetter, ok := dialer.(interface{ SetRemoteDialogEnded(func()) }); ok {
		callbackSetter.SetRemoteDialogEnded(func() {
			source.RemoteDialogEnded()
			if remoteBYE != nil {
				remoteBYE()
			}
		})
	} else {
		return nil, nil, errors.New("sip runtime does not support remote bye callback")
	}
	source = media.NewSourceSession(
		logger,
		sourceConfig,
		core.EntrypointID(entrypoint.ID),
		signaling.NewManager("127.0.0.1", dialer, nil),
		openwebnet.NewAVClient(logger),
		media.NewVideoRTPReceiver(logger, func(packet *rtp.Packet) {
			if sourceLive.Load() && videoPacket != nil {
				videoPacket(packet)
			}
		}),
		media.NewAudioRTPReceiver(logger, func(packet *rtp.Packet) {
			if sourceLive.Load() && audioPacket != nil {
				audioPacket(packet)
			}
		}),
	)
	source.SetStartedCallback(func() { sourceLive.Store(true) })
	return source, func() {
		sourceLive.Store(false)
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := source.Close(closeCtx); err != nil {
			logger.Warn("media source close failed", "error", err)
		}
	}, nil
}

func checkUpdates(ctx context.Context, updater *system.Updater, broadcast func(), logger *slog.Logger) {
	delay := 20 * time.Second
	backoff := 2 * time.Minute
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := updater.Check(checkCtx)
		cancel()
		if errors.Is(err, system.ErrUpdateUnavailable) {
			logger.Info("companion update checks unavailable")
			return
		}
		if err != nil {
			delay = backoff
			backoff = min(backoff*2, time.Hour)
		} else {
			delay = 3 * time.Hour
			backoff = 2 * time.Minute
		}
		broadcast()
	}
}

func openConfig(path string, detectMetadata func() (config.Metadata, error)) (*config.Store, bool, error) {
	if path == "" {
		path = config.DefaultPath
	}

	metadata, err := detectMetadata()
	if err != nil {
		return nil, false, fmt.Errorf("detect device metadata: %w", err)
	}

	store, err := config.Open(path)
	if err == nil {
		if err := store.ApplyMetadata(metadata); err != nil {
			return nil, false, fmt.Errorf("apply device metadata: %w", err)
		}
		return store, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("open config: %w", err)
	}

	if _, err := config.Create(path, metadata); err != nil && !errors.Is(err, config.ErrConfigExists) {
		return nil, false, fmt.Errorf("create config: %w", err)
	}

	store, err = config.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open created config: %w", err)
	}
	if err := store.ApplyMetadata(metadata); err != nil {
		return nil, false, fmt.Errorf("apply device metadata: %w", err)
	}

	return store, true, nil
}

func serve(
	ctx context.Context,
	logger *slog.Logger,
	apiListener net.Listener,
	apiServer *http.Server,
	webUIListener net.Listener,
	webUIServer *http.Server,
) error {
	errs := make(chan error, 2)

	serveServer(logger, "api", apiListener, apiServer, errs)
	serveServer(logger, "webui", webUIListener, webUIServer, errs)
	logger.Info("application ready")

	select {
	case <-ctx.Done():
		logger.Info("application stopping", "reason", "context canceled")
		err := shutdown(apiServer, webUIServer)
		if err != nil {
			logger.Error("application shutdown failed", "error", err)
			return err
		}
		logger.Info("application stopped")
		return nil
	case err := <-errs:
		return errors.Join(err, shutdown(apiServer, webUIServer))
	}
}

func serveServer(logger *slog.Logger, name string, listener net.Listener, server *http.Server, errs chan<- error) {
	go func() {
		logger.Info("server listening", "server", name, "address", listener.Addr().String())
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("serve %s: %w", name, err)
		}
	}()
}

func shutdown(servers ...*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var errs []error
	for _, server := range servers {
		if err := server.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

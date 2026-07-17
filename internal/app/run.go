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
	"os/exec"
	"path/filepath"
	"strings"
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
	if configPath == "" {
		configPath = config.DefaultPath
	}

	configStore, created, err := openConfig(configPath, detectMetadata)
	if err != nil {
		return err
	}

	if created {
		logger.Info("created initial configuration", "path", configPath, "device_id", configStore.Snapshot().Companion.DeviceID)
	}
	if setLogLevel != nil {
		if err := setLogLevel(configStore.Snapshot().Companion.LogLevel); err != nil {
			return fmt.Errorf("set log level: %w", err)
		}
	}

	projector := core.NewProjector()
	openWebNetTrace := openwebnet.NewTrace(0)
	openWebNetControl := openwebnet.NewControl(configStore.Snapshot().Companion.Entrypoints, openWebNetTrace)
	rtspServer, err := media.NewRTSPServer(logger, media.DefaultRTSPAddress, configStore.Snapshot().Companion.Entrypoints, func(entrypoint config.Entrypoint, events media.SourceEvents) (media.ManagedSource, func(), error) {
		return newBridgeSource(configStore.Snapshot(), logger, entrypoint, events)
	})
	if err != nil {
		return fmt.Errorf("create rtsp server: %w", err)
	}
	if err := rtspServer.Start(ctx); err != nil {
		return err
	}
	defer func() {
		if err := rtspServer.Close(); err != nil {
			logger.Warn("close rtsp server", "error", err)
		}
	}()

	allowedServices := []string{}

	for name, svc := range configStore.Snapshot().System.Services {
		if svc.Enabled {
			allowedServices = append(allowedServices, name)
		}
	}

	rt := system.NewRuntimeControl(nil, system.NewRebootAdapter(nil), allowedServices)

	build := system.CurrentBuildInfo()
	updatePolicy := func() system.UpdatePolicy {
		cfg := configStore.Snapshot().System
		return system.UpdatePolicy{Enabled: cfg.UpdateEnabled, Exposed: cfg.UpdateExposed, DataDir: system.CompanionDataDir}
	}
	var updateSource system.ReleaseSource
	if strings.TrimSpace(build.ReleaseRepo) != "" {
		updateSource = system.NewGitHubReleaseClient(&http.Client{Timeout: 30 * time.Second}, "https://api.github.com/repos/"+build.ReleaseRepo+"/releases/latest")
	}
	restartCompanion := func(ctx context.Context) error {
		return exec.CommandContext(ctx, "/etc/init.d/companion", "restart").Run()
	}
	updater := system.NewUpdater(updateSource, build, updatePolicy, restartCompanion)

	webrtc := media.NewWebRTCService(nil, nil, nil, nil)
	snapshot := media.NewSnapshotService(nil, nil, nil)

	authStore := auth.NewStore(configStore)
	mdns := discovery.NewService(nil)

	server := api.NewServer(authStore, configStore, projector, logger)
	server.SetEntrypoints(openWebNetControl)
	server.SetAudio(openWebNetControl)
	server.SetVoicemail(openWebNetControl)
	server.SetWebRTC(webrtc)
	server.SetSnapshot(snapshot)
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
	webUI := webui.New(configStore, logger, restartCompanion, setLogLevel)
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
			logger.Warn("apply openwebnet event", "event", event.Type(), "error", err)
			return
		}
		server.BroadcastState()
		server.BroadcastEvent(map[string]any{"type": event.Type()})
	}
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
			logger.Error("openwebnet listener stopped", "error", err)
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
				DeviceID:   cfg.Companion.DeviceID,
				Name:       cfg.Companion.Name,
				Model:      cfg.Companion.Model,
				NeedsClaim: authStore.NeedsClaim(),
				Port:       8080,
				Interfaces: []net.Interface{iface},
			}, nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Error("mDNS stopped", "error", err)
		}
	}()
	return serve(ctx, logger, apiListener, apiServer, webUIListener, webUIServer)
}

func newBridgeSource(cfg config.Config, logger *slog.Logger, entrypoint config.Entrypoint, events media.SourceEvents) (media.ManagedSource, func(), error) {
	var bridge *media.AudioBridge
	source, closeSource, err := newSource(cfg, logger, entrypoint, events.VideoRTP, func(packet *rtp.Packet) {
		if err := bridge.WriteIntercomSpeex(packet); err != nil {
			logger.Warn("bridge intercom speex", "error", err)
		}
	}, events.RemoteBYE)
	if err != nil {
		return nil, nil, err
	}
	bridge = media.NewAudioBridge(media.NewGStreamerAudioBridge(filepath.Join(system.CompanionDataDir, "gst"), logger), events.AudioRTP, nil, logger, events.Failed)
	return &bridgeSource{source: source, bridge: bridge}, closeSource, nil
}

type bridgeSource struct {
	source *media.SourceSession
	bridge *media.AudioBridge
}

func (s *bridgeSource) Start(ctx context.Context) error {
	if err := s.bridge.Start(ctx); err != nil {
		return fmt.Errorf("start audio bridge: %w", err)
	}
	if err := s.source.Start(ctx); err != nil {
		_ = s.bridge.Stop()
		return err
	}
	return nil
}

func (s *bridgeSource) Close(ctx context.Context) error {
	err := s.source.Close(ctx)
	if bridgeErr := s.bridge.Stop(); bridgeErr != nil && err == nil {
		err = bridgeErr
	}
	return err
}

func newSource(cfg config.Config, logger *slog.Logger, entrypoint config.Entrypoint, videoPacket, audioPacket func(*rtp.Packet), remoteBYE func()) (*media.SourceSession, func(), error) {
	sourceConfig, err := media.ResolveSourceConfig(cfg.Companion.Model, entrypoint)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve media source: %w", err)
	}
	logger.Info("media source configuration resolved",
		"component", "media.source",
		"model", sourceConfig.Model,
		"entrypoint_id", entrypoint.ID,
		"dev_addr", sourceConfig.DevAddr,
		"high_res_video", sourceConfig.HighResVideo,
		"target", sourceConfig.Target,
	)

	var source *media.SourceSession
	dialer, err := signaling.NewStreamDialer(signaling.StreamDialerConfig{
		Target: sourceConfig.Target,
		Domain: signaling.DiscoverFlexisipDomain(),
		Logger: logger,
		RemoteDialogEnded: func() {
			source.RemoteDialogEnded()
			if remoteBYE != nil {
				remoteBYE()
			}
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create sip dialer: %w", err)
	}
	source = media.NewSourceSession(
		logger,
		sourceConfig,
		core.EntrypointID(entrypoint.ID),
		signaling.NewManager("127.0.0.1", dialer, nil),
		openwebnet.NewAVClient(logger),
		media.NewVideoRTPReceiver(logger, videoPacket),
		media.NewAudioRTPReceiver(logger, audioPacket),
	)
	return source, func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := source.Close(closeCtx); err != nil {
			logger.Warn("close media source", "error", err)
		}
		if err := dialer.Close(); err != nil {
			logger.Warn("close diagnostic sip dialer", "error", err)
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
			return
		}
		if err != nil {
			logger.Debug("check companion update", "error", err)
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

	select {
	case <-ctx.Done():
		return shutdown(apiServer, webUIServer)
	case err := <-errs:
		return errors.Join(err, shutdown(apiServer, webUIServer))
	}
}

func serveServer(logger *slog.Logger, name string, listener net.Listener, server *http.Server, errs chan<- error) {
	go func() {
		logger.Info(name+" listening", "addr", listener.Addr().String())
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

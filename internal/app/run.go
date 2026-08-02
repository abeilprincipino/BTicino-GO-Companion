package app

import (
	"bticino-go-companion/internal/api"
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/diagnostics"
	"bticino-go-companion/internal/discovery"
	"bticino-go-companion/internal/homekit"
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
	"sync"
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

	migrateSIPSection(configStore, appLogger, configPath)

	appLogger.Info("application starting", "config_path", configPath)

	if setLogLevel != nil {
		if err := setLogLevel(configStore.Snapshot().Logging.Level); err != nil {
			return fmt.Errorf("set log level: %w", err)
		}
	}

	runtime, err := newRuntime(ctx, configStore, logger)
	if err != nil {
		return err
	}
	defer runtime.close(appLogger)

	projector := runtime.projector
	openWebNetTrace := runtime.trace
	openWebNetControl := runtime.control
	rtspServer := runtime.rtspServer
	updater := runtime.updater
	authStore := runtime.authStore
	mdns := runtime.mdns
	homeKit := runtime.homeKit
	server := runtime.server
	diagnosticService := runtime.diagnosticService
	rt := runtime.runtime

	restartCompanion := func(ctx context.Context) error { return rt.Restart(ctx, "companion") }

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
	webUI.SetHomeKit(homeKit)
	webUI.SetDiagnostics(diagnosticService)
	webUI.SetUpdate(updater)
	webUIServer := &http.Server{
		Handler:           webUI.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go checkUpdates(ctx, updater, server.BroadcastState, logger)

	applyEvent := newEventApplier(projector, homeKit, server, logger)

	// The sink first, the inbound handler second, and never the other way
	// round. Manager.SetEvents drops what is published while the sink is still
	// nil, so an INVITE served in between would publish IncomingCallStarted
	// into nothing: the manager would believe it is ringing while the projector
	// had never heard of the call, and every later inbound call would be
	// rejected as busy for good.
	//
	// The dialer's SIP listener is already running by now — newRuntime starts it
	// — but until SetInboundHandler is called onInvite has no handler and
	// answers 503 Service Unavailable. Refusing the one call that can arrive in
	// this startup window is honest and self-correcting; dropping its event is
	// neither.
	runtime.calls.SetEvents(eventSinkFunc(applyEvent))

	if runtime.inboundSIP {
		runtime.dialer.SetInboundHandler(runtime.calls)
	}

	refreshVoicemail := newVoicemailRefresher(openWebNetControl, projector, applyEvent)
	server.SetVoicemailRefresh(refreshVoicemail)
	rtspServer.Coordinator().SetStateObserver(newStreamStateObserver(projector, applyEvent))

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

	voicemailRefreshRequests := make(chan struct{}, 1)
	requestVoicemailRefresh := func() {
		select {
		case voicemailRefreshRequests <- struct{}{}:
		default:
		}
	}

	listener.SetMessageObserver(func(message openwebnet.Message) {
		if !strings.EqualFold(strings.TrimSpace(message.System), "aswm") || strings.TrimSpace(message.Raw) != openwebnet.FrameACK {
			return
		}

		requestVoicemailRefresh()
	})

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-voicemailRefreshRequests:
			}

			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}

			draining := true
			for draining {
				select {
				case <-voicemailRefreshRequests:
				default:
					draining = false
				}
			}

			refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := refreshVoicemail(refreshCtx)

			cancel()

			if err != nil && ctx.Err() == nil {
				logger.Debug("openwebnet voicemail refresh failed", "error", err)
			}
		}
	}()
	go func() {
		if err := listener.Run(ctx, applyEvent); err != nil && ctx.Err() == nil {
			logger.Error("openwebnet listener stopped", "component", "openwebnet.listener", "error", err)
		}
	}()
	go func() {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		if muted, err := openWebNetControl.AudioMutedStatus(probeCtx); err != nil {
			logger.Debug("openwebnet initial audio state unavailable", "error", err)
		} else if muted {
			applyEvent(core.AudioMuted{})
		} else {
			applyEvent(core.AudioUnmuted{})
		}

		if _, err := refreshVoicemail(probeCtx); err != nil {
			logger.Debug("openwebnet initial voicemail state unavailable", "error", err)
		}

		if err := homeKit.Run(ctx, system.CompanionDataDir, logger); err != nil && ctx.Err() == nil {
			logger.Error("homekit bridge stopped", "component", "homekit", "error", err)
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

type applicationRuntime struct {
	projector         *core.Projector
	trace             *openwebnet.Trace
	control           *openwebnet.Control
	rtspServer        *media.RTSPServer
	updater           *system.Updater
	webrtc            *media.WebRTCService
	authStore         *auth.Store
	mdns              *discovery.Service
	homeKit           *homekit.Manager
	server            *api.Server
	diagnosticService *diagnostics.Service
	runtime           *system.RuntimeControl
	dialer            interface {
		signaling.StreamDialer
		SetInboundHandler(signaling.InboundHandler)
		Close() error
	}

	// calls is the process-wide SIP dialog owner, shared by the API and by
	// every media source.
	calls *signaling.Manager

	// inboundSIP records whether the dialer was built with inbound handling, so
	// Run installs the inbound handler only where an INVITE can actually arrive.
	inboundSIP bool
}

// newInboundEntrypointResolver attributes an inbound SIP call to an entrypoint.
// The in-flight physical ring is the strongest signal; a single configured
// entrypoint is the safe fallback. Ambiguity is reported as "unattributable"
// so the manager rejects the call rather than inventing state.
//
// It runs with the manager's state lock held, so it is a side-effect-free
// lookup and nothing more: two snapshot reads, each taking a lock of its own
// that no path holds while entering the manager, and no call back into the
// manager, which would deadlock on that lock.
func newInboundEntrypointResolver(entrypoints func() []config.Entrypoint, projector *core.Projector) signaling.EntrypointResolver {
	return func() (core.EntrypointID, string) {
		configured := entrypoints()

		if ring := projector.Snapshot().PhysicalRing; ring != nil {
			for _, entrypoint := range configured {
				if core.EntrypointID(entrypoint.ID) == ring.EntrypointID {
					return ring.EntrypointID, entrypoint.DevAddr
				}
			}
		}

		if len(configured) == 1 {
			return core.EntrypointID(configured[0].ID), configured[0].DevAddr
		}

		return "", ""
	}
}

func newRuntime(ctx context.Context, configStore *config.Store, logger *slog.Logger) (*applicationRuntime, error) {
	initialConfig := configStore.Snapshot()
	if len(initialConfig.Companion.Entrypoints) == 0 {
		return nil, errors.New("create sip runtime: no entrypoints configured")
	}

	mediaConfig, err := media.ResolveSourceConfig(initialConfig.Companion.Model, initialConfig.Companion.Entrypoints[0])
	if err != nil {
		return nil, fmt.Errorf("resolve sip runtime source: %w", err)
	}

	// The whole SIP block is handed to the dialer, not just the target: the
	// identity it registers with, where it listens and — above all — Inbound,
	// without which no inbound handler is registered at all and the doorbell
	// call never reaches the companion.
	sipConfig := initialConfig.Companion.SIP

	// A configured domain wins over discovery. Discovery probes the device for
	// the Flexisip realm and is the fallback for an installation that has not
	// been told one; an explicit value is an operator overriding exactly that.
	domain := strings.TrimSpace(sipConfig.Domain)
	if domain == "" {
		domain = signaling.DiscoverFlexisipDomain()
	}

	dialer, err := signaling.NewStreamDialer(signaling.StreamDialerConfig{
		Target:    mediaConfig.Target,
		Domain:    domain,
		From:      sipConfig.From,
		AuthUser:  sipConfig.AuthUser,
		AuthPass:  sipConfig.AuthPass,
		Transport: sipConfig.Transport,
		Listen:    sipConfig.Listen,
		Inbound:   sipConfig.Inbound,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create sip runtime: %w", err)
	}

	// The projector is built here, before the source factory closure that
	// captures it, because the resolver below reads its snapshot. The closure
	// only captures it; it does not dereference it at construction time.
	projector := core.NewProjector()

	// One manager for the whole process. It owns the single inbound and the
	// single outbound dialog, so the API and every media source have to be
	// looking at the same one: a per-source manager would answer a call whose
	// dialog another manager holds.
	//
	// The sink is nil here and installed in Run, once the projector-backed
	// applier exists. Nothing can publish into it before then, because the
	// inbound handler is installed in Run too, after the sink.
	calls := signaling.NewManager("127.0.0.1", dialer, nil, newInboundEntrypointResolver(
		func() []config.Entrypoint { return configStore.Snapshot().Companion.Entrypoints },
		projector,
	))

	trace := openwebnet.NewTrace(0)
	control := openwebnet.NewControl(initialConfig.Companion.Entrypoints, trace)
	snapshots := media.NewSnapshotManager(system.CompanionDataDir, logger)

	rtspServer, err := media.NewRTSPServer(logger, media.DefaultRTSPAddress, initialConfig.Companion.Entrypoints, func(entrypoint config.Entrypoint, events media.SourceEvents) (media.ManagedSource, func(), error) {
		return newBridgeSource(configStore.Snapshot(), logger, dialer, calls, entrypoint, events, snapshots)
	})
	if err != nil {
		_ = dialer.Close()
		return nil, fmt.Errorf("create rtsp server: %w", err)
	}

	// The coordinator refuses a lease for a stream it sees as externally owned.
	// On an inbound call the intercom starts its AV while ringing, so that flag
	// is set well before the user answers and the answered call would never get
	// a lease. The probe tells the coordinator when the external stream is in
	// fact the companion's own answered call. See
	// docs/superpowers/specs/2026-08-02-inbound-call-stream-ownership-design.md.
	rtspServer.Coordinator().SetAnsweredCallProbe(calls.HasAnsweredInboundCall)

	if err := rtspServer.Start(ctx); err != nil {
		_ = dialer.Close()
		return nil, err
	}

	webrtc, err := media.NewWebRTCService(rtspServer.Coordinator(), initialConfig.Companion.Entrypoints)
	if err != nil {
		_ = rtspServer.Close()
		_ = dialer.Close()

		return nil, fmt.Errorf("create WebRTC service: %w", err)
	}

	allowed := make([]string, 0)

	for name, service := range initialConfig.System.Services {
		if service.Enabled {
			allowed = append(allowed, name)
		}
	}

	rt := system.NewRuntimeControl(system.NewInitServiceAdapter(nil), system.NewRebootAdapter(nil), allowed)
	build := system.CurrentBuildInfo()
	policy := func() system.UpdatePolicy {
		cfg := configStore.Snapshot().System
		return system.UpdatePolicy{Enabled: cfg.UpdateEnabled, Exposed: cfg.UpdateExposed, DataDir: system.CompanionDataDir}
	}

	var source system.ReleaseSource
	if strings.TrimSpace(build.ReleaseRepo) != "" {
		source = system.NewGitHubReleaseClient(&http.Client{Timeout: 30 * time.Second}, "https://api.github.com/repos/"+build.ReleaseRepo+"/releases/latest")
	}

	updater := system.NewUpdater(source, build, policy, func(ctx context.Context) error { return rt.Restart(ctx, "companion") })
	authStore := auth.NewStore(configStore)
	authStore.SetLogger(logger.With("component", "auth"))

	homeKit, err := homekit.NewManager(configStore)
	if err != nil {
		_ = webrtc.Shutdown()
		_ = rtspServer.Close()
		_ = dialer.Close()

		return nil, fmt.Errorf("create homekit manager: %w", err)
	}

	homeKit.SetControllers(control, control, control)
	homeKit.SetStreamCoordinator(rtspServer.Coordinator())
	homeKit.SetSnapshotProvider(snapshots)

	server := api.NewServer(authStore, configStore, projector, logger)
	server.SetEntrypoints(control)
	server.SetAudio(control)
	server.SetVoicemail(control)
	server.SetWebRTC(webrtc)
	server.SetSnapshot(snapshots)
	snapshots.SetOnCaptured(server.BroadcastState)
	server.SetRuntime(rt)
	server.SetUpdate(updater)
	diagnosticService := diagnostics.New(control, initialConfig.Companion.Model, server.BroadcastState)
	server.SetDiagnostics(diagnosticService)

	// The answer and hangup routes are exposed only where a call can actually
	// arrive. Left registered on an outbound-only installation they would offer
	// the user a button that can never do anything but fail.
	if sipConfig.Inbound {
		server.SetCall(calls)
	}

	return &applicationRuntime{
		projector:         projector,
		trace:             trace,
		control:           control,
		rtspServer:        rtspServer,
		updater:           updater,
		webrtc:            webrtc,
		authStore:         authStore,
		mdns:              discovery.NewService(nil),
		homeKit:           homeKit,
		server:            server,
		diagnosticService: diagnosticService,
		runtime:           rt,
		dialer:            dialer,
		calls:             calls,
		inboundSIP:        sipConfig.Inbound,
	}, nil
}

func (r *applicationRuntime) close(logger *slog.Logger) {
	if err := r.webrtc.Shutdown(); err != nil {
		logger.Warn("webrtc service shutdown failed", "error", err)
	}

	if err := r.rtspServer.Close(); err != nil {
		logger.Warn("RTSP server close failed", "error", err)
	}

	if err := r.dialer.Close(); err != nil {
		logger.Warn("sip runtime close failed", "error", err)
	}
}

func newVoicemailRefresher(control *openwebnet.Control, projector *core.Projector, applyEvent func(core.Event)) func(context.Context) (bool, error) {
	var mu sync.Mutex

	return func(ctx context.Context) (bool, error) {
		mu.Lock()
		defer mu.Unlock()

		status, err := control.VoicemailStatus(ctx)
		if err != nil {
			if errors.Is(err, openwebnet.ErrVoicemailUnavailable) {
				if projector.Snapshot().Voicemail != nil {
					applyEvent(core.VoicemailUnavailable{})
				}

				return false, nil
			}

			return false, fmt.Errorf("read voicemail status: %w", err)
		}

		current := projector.Snapshot().Voicemail
		if current != nil && current.Enabled == status.Enabled {
			return true, nil
		}

		if status.Enabled {
			applyEvent(core.VoicemailEnabled{})
		} else {
			applyEvent(core.VoicemailDisabled{})
		}

		return true, nil
	}
}

// eventSinkFunc adapts the applier closure to signaling.EventSink.
type eventSinkFunc func(core.Event)

func (f eventSinkFunc) Publish(event core.Event) { f(event) }

func newEventApplier(projector *core.Projector, homeKit *homekit.Manager, server *api.Server, logger *slog.Logger) func(core.Event) {
	return func(event core.Event) {
		if _, err := projector.Apply(event); err != nil && !errors.Is(err, core.ErrInvalidTransition) {
			logger.Warn("openwebnet event apply failed", "event_type", event.Type(), "error", err)
			return
		}

		homeKit.Sync(projector.Snapshot())
		server.BroadcastState()
		server.BroadcastEvent(map[string]any{"type": event.Type()})
	}
}

func newStreamStateObserver(projector *core.Projector, applyEvent func(core.Event)) media.StreamStateObserver {
	return func(snapshot media.StreamSnapshot) {
		streamID := core.StreamID(fmt.Sprintf("media-%d", snapshot.LeaseID))
		switch snapshot.Owner {
		case media.StreamOwnerCompanion:
			applyEvent(core.PreviewStarted{StreamID: streamID, EntrypointID: core.EntrypointID(snapshot.EntrypointID)})
		case media.StreamOwnerIdle:
			preview := projector.Snapshot().PreviewStream
			if preview != nil && strings.HasPrefix(string(preview.StreamID), "media-") {
				applyEvent(core.PreviewStopped{StreamID: preview.StreamID})
			}
		}
	}
}

func newBridgeSource(cfg config.Config, logger *slog.Logger, dialer signaling.StreamDialer, calls *signaling.Manager, entrypoint config.Entrypoint, events media.SourceEvents, snapshots *media.SnapshotManager) (media.ManagedSource, func(), error) {
	backchannel, err := media.NewUDPBackchannel("")
	if err != nil {
		return nil, nil, fmt.Errorf("create udp backchannel: %w", err)
	}

	attempt := snapshots.Arm(entrypoint.ID)

	var bridge *media.AudioBridge

	source, closeSource, err := newSource(cfg, logger, dialer, calls, entrypoint, func(packet *rtp.Packet) {
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

func newSource(cfg config.Config, logger *slog.Logger, dialer signaling.StreamDialer, calls *signaling.Manager, entrypoint config.Entrypoint, videoPacket, audioPacket func(*rtp.Packet), remoteBYE func()) (*media.SourceSession, func(), error) {
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

	var (
		source     *media.SourceSession
		sourceLive atomic.Bool
	)

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
		calls,
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

// migrateSIPSection persists the companion.sip section on installations whose
// config.yaml was written before that section existed, so the installer finds an
// inbound: key to enable. It is a no-op once the section is on disk. A failure
// costs only the migration: the in-memory configuration already carries the SIP
// defaults, so the companion must keep starting.
func migrateSIPSection(store *config.Store, logger *slog.Logger, path string) {
	migrated, err := store.EnsureSIPSection()
	if err != nil {
		logger.Warn("persisting the sip configuration section failed", "path", path, "error", err)

		return
	}

	if migrated {
		logger.Info("persisted the missing sip configuration section", "path", path)
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

package app

import (
	"bticino-go-companion/internal/api"
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
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
	"time"
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

	allowedServices := []string{}

	for name, svc := range configStore.Snapshot().System.Services {
		if svc.Enabled {
			allowedServices = append(allowedServices, name)
		}
	}

	rt := system.NewRuntimeControl(nil, nil, allowedServices)

	updater := system.NewUpdater(nil, nil, nil)

	webrtc := media.NewWebRTCService(nil, nil, nil, nil)
	snapshot := media.NewSnapshotService(nil, nil, nil)

	commands := api.NewProjectorCommands(
		projector,
		nil,
		nil,
		nil,
		rt,
		updater,
		webrtc,
		snapshot,
	)

	authStore := auth.NewStore(configStore)

	server := api.NewServer(authStore, configStore, projector, commands, logger)
	server.SetEntrypoints(nil)
	server.SetAudio(nil)
	server.SetVoicemail(nil)
	server.SetWebRTC(webrtc)
	server.SetSnapshot(snapshot)
	server.SetRuntime(rt)
	server.SetUpdate(updater)

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
	webUIServer := &http.Server{
		Handler: webui.New(configStore, logger, func(ctx context.Context) error {
			return exec.CommandContext(ctx, "/etc/init.d/companion", "restart").Run()
		}, setLogLevel).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return serve(ctx, logger, apiListener, apiServer, webUIListener, webUIServer)
}

func openConfig(path string, detectMetadata func() (config.Metadata, error)) (*config.Store, bool, error) {
	if path == "" {
		path = config.DefaultPath
	}

	store, err := config.Open(path)
	if err == nil {
		return store, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("open config: %w", err)
	}

	metadata, err := detectMetadata()
	if err != nil {
		return nil, false, fmt.Errorf("detect device metadata: %w", err)
	}

	if _, err := config.Create(path, metadata); err != nil && !errors.Is(err, config.ErrConfigExists) {
		return nil, false, fmt.Errorf("create config: %w", err)
	}

	store, err = config.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open created config: %w", err)
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
		logger.Info("http server listening", "server", name, "addr", listener.Addr().String())
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

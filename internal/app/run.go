package app

import (
	"bticino-go-companion/internal/api"
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/system"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

func Run(ctx context.Context, configPath string, logger *slog.Logger) error {
	configStore, err := config.Open(configPath)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
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

	server := api.NewServer(authStore, configStore, projector, commands)
	server.SetEntrypoints(nil)
	server.SetAudio(nil)
	server.SetVoicemail(nil)
	server.SetWebRTC(webrtc)
	server.SetSnapshot(snapshot)
	server.SetRuntime(rt)
	server.SetUpdate(updater)

	mux := http.NewServeMux()
	mux.Handle("/api/v3/", http.StripPrefix("/api/v3", server.Handler()))

	httpServer := &http.Server{
		Addr:              ":80",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("http server listening", slog.String("addr", ":80"))

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", slog.Any("error", err))
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}

	return nil
}

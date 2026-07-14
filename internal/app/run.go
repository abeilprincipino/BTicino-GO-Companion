package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"bticino-go-companion/internal/api"
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/discovery"
	"bticino-go-companion/internal/system"
	"bticino-go-companion/internal/webui"
)

func Run(ctx context.Context, configPath string, logger *slog.Logger) error {
	store, err := openConfig(configPath)
	if err != nil {
		return err
	}

	authStore := auth.NewStore(store)
	projector := core.NewProjector()
	commands := api.NewProjectorCommands(projector)
	apiServer := api.NewServer(authStore, store, projector, commands)
	webuiServer := webui.New(store, logger, restartCompanion)
	discoveryService := discovery.NewService(nil)

	apiHTTPServer := &http.Server{
		Addr:         ":8080",
		Handler:      apiServer.Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	webuiHTTPServer := &http.Server{
		Addr:         ":80",
		Handler:      webuiServer.Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	cfg := store.Snapshot()
	registration, err := discoveryService.Advertise(discovery.Advertisement{
		DeviceID:   cfg.Companion.DeviceID,
		Name:       cfg.Companion.Name,
		Model:      cfg.Companion.Model,
		NeedsClaim: cfg.Auth.BearerToken == "",
		Port:       8080,
	})
	if err != nil {
		logger.Warn("mdns advertisement failed", "error", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("api listening", "address", apiHTTPServer.Addr)
		if err := apiHTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve api: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("webui listening", "address", webuiHTTPServer.Addr)
		if err := webuiHTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("serve webui: %w", err)
		}
	}()

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := apiHTTPServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown api", "error", err)
		}
		if err := webuiHTTPServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown webui", "error", err)
		}
		if registration != nil {
			registration.Shutdown()
		}
	}

	select {
	case err := <-errCh:
		shutdown()
		wg.Wait()
		return err
	case <-ctx.Done():
		shutdown()
		wg.Wait()
		return nil
	}
}

func openConfig(configPath string) (*config.Store, error) {
	if configPath == "" {
		configPath = config.DefaultPath
	}
	store, err := config.Open(configPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		metadata, err := system.DetectMetadata()
		if err != nil {
			return nil, err
		}
		if _, err := config.Create(configPath, metadata); err != nil {
			return nil, fmt.Errorf("create first-boot config: %w", err)
		}
		store, err = config.Open(configPath)
		if err != nil {
			return nil, fmt.Errorf("open first-boot config: %w", err)
		}
	}
	return store, nil
}

func restartCompanion(context.Context) error {
	return exec.Command("/etc/init.d/companion", "restart").Start()
}

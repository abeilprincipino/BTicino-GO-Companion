package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/system"
	"bticino-go-companion/internal/webui"
)

func Run(ctx context.Context, configPath string, logger *slog.Logger) error {
	if configPath == "" {
		configPath = config.DefaultPath
	}
	store, err := config.Open(configPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		metadata, err := system.DetectMetadata()
		if err != nil {
			return err
		}
		cfg, err := config.Create(configPath, metadata)
		if err != nil {
			return fmt.Errorf("create first-boot config: %w", err)
		}
		logger.Info("created first-boot config", "path", configPath, "claim_code", cfg.Auth.ClaimCode)
		store, err = config.Open(configPath)
		if err != nil {
			return fmt.Errorf("open first-boot config: %w", err)
		}
	}

	logger.Info("companion configuration is ready", "path", configPath)
	server := &http.Server{
		Addr:         ":80",
		Handler:      webui.New(store, logger, restartCompanion).Handler(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown webui", "error", err)
		}
	}()
	logger.Info("webui listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve webui: %w", err)
	}
	return nil
}

func restartCompanion(context.Context) error {
	return exec.Command("/etc/init.d/companion", "restart").Start()
}

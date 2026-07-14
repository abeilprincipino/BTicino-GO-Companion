package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/system"
)

func Run(ctx context.Context, configPath string, logger *slog.Logger) error {
	if configPath == "" {
		configPath = config.DefaultPath
	}
	if _, err := config.Open(configPath); err != nil {
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
	}

	logger.Info("companion configuration is ready", "path", configPath)
	<-ctx.Done()
	return nil
}

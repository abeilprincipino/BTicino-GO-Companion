package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"bticino-go-companion/internal/app"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to config.yaml")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Run(ctx, configPath, logger); err != nil {
		logger.Error("companion stopped", "error", err)
		os.Exit(1)
	}
}

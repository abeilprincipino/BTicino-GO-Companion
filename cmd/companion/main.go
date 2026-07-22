package main

import (
	"bticino-go-companion/internal/app"
	"bticino-go-companion/internal/logging"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run())
}

func run() int {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to config.yaml")
	flag.Parse()

	runtime, err := logging.New("info")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "companion logging initialization failed: %v\n", err)

		return 1
	}
	defer func() {
		if closeErr := runtime.Close(); closeErr != nil {
			runtime.Logger.Warn("logging shutdown failed", "error", closeErr)
		}
	}()

	slog.SetDefault(runtime.Logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := app.Run(ctx, configPath, runtime.Logger, runtime.SetLevel); err != nil {
		runtime.Logger.Error("application stopped unexpectedly", "component", "app", "error", err)
		return 1
	}

	return 0
}

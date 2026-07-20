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
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to config.yaml")
	flag.Parse()

	runtime, err := logging.New("info")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "companion logging initialization failed: %v\n", err)
		os.Exit(1)
	}
	defer runtime.Close()
	slog.SetDefault(runtime.Logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := app.Run(ctx, configPath, runtime.Logger, runtime.SetLevel); err != nil {
		runtime.Logger.Error("application stopped unexpectedly", "component", "app", "error", err)
		os.Exit(1)
	}
}

package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"bticino-go-companion/internal/app"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "", "path to companion config json")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := app.Run(ctx, cfgPath, log.Default()); err != nil {
		log.Fatalf("companion failed: %v", err)
	}
}

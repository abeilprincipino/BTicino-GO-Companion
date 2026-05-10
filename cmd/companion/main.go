package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bticino-go-companion/internal/adapters/http/v2"
	"bticino-go-companion/internal/adapters/openwebnet"
	"bticino-go-companion/internal/adapters/sip"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/event"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/runtime"
	"bticino-go-companion/internal/services/state"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "", "path to companion config json")
	flag.Parse()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	projector := state.NewProjector(cfg.Entrypoints)
	eventBroker := events.New(512)
	normalizer := events.NewNormalizer(cfg.Entrypoints)
	runtimeStatus := runtime.New(cfg.MediaSIPEnabled, cfg.OpenWebNetEnabled)
	publish := func(ev event.Envelope) {
		normalized := normalizer.Normalize(ev)
		enriched := projector.Apply(normalized)
		eventBroker.Publish(enriched)
	}

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      nil,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sipManager := sipadapter.NewManager(cfg, log.Default())
	sipManager.SetEventSink(func(ev event.Envelope) {
		publish(ev)
	})
	if err := sipManager.Start(ctx); err != nil {
		runtimeStatus.SetSIPReady(false, err.Error())
		log.Fatalf("start sip manager: %v", err)
	}
	runtimeStatus.SetSIPReady(true, "")
	defer func() {
		if err := sipManager.Close(); err != nil {
			log.Printf("sip manager close warning: %v", err)
		}
	}()

	commandClient := openwebnet.NewCommandClient(cfg)
	controlService := control.New(cfg.Entrypoints, sipManager, commandClient, func(ev event.Envelope) {
		publish(ev)
	})
	runtimeStatus.SetControlReady(true, "")
	router := v2.NewRouter(projector, controlService, eventBroker, runtimeStatus)
	srv.Handler = router.Handler()

	if cfg.OpenWebNetEnabled {
		listener := openwebnet.NewListener(cfg.OpenWebNetGroup, cfg.OpenWebNetListenPort, cfg.OpenWebNetReadBuffer)
		runtimeStatus.SetOpenWebNetReady(true, "")
		go func() {
			err := listener.Run(ctx, func(ev event.Envelope) {
				publish(ev)
			})
			if err != nil {
				runtimeStatus.SetOpenWebNetReady(false, err.Error())
				log.Printf("openwebnet listener stopped: %v", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("companion v2 api listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server failed: %v", err)
	}
}

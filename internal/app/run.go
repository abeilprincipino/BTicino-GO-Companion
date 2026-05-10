package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"bticino-go-companion/internal/adapters/http/v2"
	"bticino-go-companion/internal/adapters/openwebnet"
	"bticino-go-companion/internal/adapters/rtsp"
	"bticino-go-companion/internal/adapters/sip"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/event"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/media"
	"bticino-go-companion/internal/services/runtime"
	"bticino-go-companion/internal/services/state"
)

func Run(ctx context.Context, cfgPath string, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	projector := state.NewProjector(cfg.Entrypoints)
	eventBroker := events.New(512)
	normalizer := events.NewNormalizer(cfg.Entrypoints)
	validator := events.NewValidator()
	runtimeStatus := runtime.New(cfg.MediaSIPEnabled, cfg.OpenWebNetEnabled)

	publish := func(ev event.Envelope) {
		normalized := normalizer.Normalize(ev)
		if err := validator.Validate(normalized); err != nil {
			logger.Printf("event validation warning: %v type=%s source=%s", err, normalized.Type, normalized.Source)
			normalized = event.Envelope{
				Type:   event.TypeEventInvalid,
				TS:     normalized.TS,
				Source: event.SourceSystem,
				Payload: map[string]any{
					"error": err.Error(),
					"original": map[string]any{
						"type":          normalized.Type,
						"source":        normalized.Source,
						"entrypoint_id": normalized.EntrypointID,
						"raw":           normalized.Raw,
					},
				},
				Raw: normalized.Raw,
			}
		}
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

	sipManager := sipadapter.NewManager(cfg, logger)
	sipManager.SetEventSink(func(ev event.Envelope) {
		publish(ev)
	})
	if err := sipManager.Start(ctx); err != nil {
		runtimeStatus.SetSIPReady(false, err.Error())
		return fmt.Errorf("start sip manager: %w", err)
	}
	runtimeStatus.SetSIPReady(true, "")
	defer func() {
		if err := sipManager.Close(); err != nil {
			logger.Printf("sip manager close warning: %v", err)
		}
	}()

	commandClient := openwebnet.NewCommandClient(cfg)
	mediaService := media.NewService(sipManager)

	if cfg.MediaRTSPEnabled {
		rtspServer := rtspadapter.NewServer(cfg, logger, mediaService)
		if err := rtspServer.Start(ctx); err != nil {
			return fmt.Errorf("start rtsp server: %w", err)
		}
	}

	controlService := control.New(cfg.Entrypoints, mediaService, commandClient, func(ev event.Envelope) {
		publish(ev)
	})
	runtimeStatus.SetControlReady(true, "")

	router := v2.NewRouter(projector, controlService, eventBroker, runtimeStatus)
	srv.Handler = router.Handler()

	if cfg.OpenWebNetEnabled {
		listener := openwebnet.NewListener(cfg.OpenWebNetGroup, cfg.OpenWebNetListenPort, cfg.OpenWebNetReadBuffer)
		runtimeStatus.SetOpenWebNetReady(true, "")
		go func() {
			if err := listener.Run(ctx, func(ev event.Envelope) { publish(ev) }); err != nil {
				runtimeStatus.SetOpenWebNetReady(false, err.Error())
				logger.Printf("openwebnet listener stopped: %v", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Printf("companion v2 api listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server failed: %w", err)
	}
	return nil
}

package v2

import (
	"net/http"

	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/runtime"
	"bticino-go-companion/internal/services/state"
)

type Router struct {
	state   *state.Projector
	control *control.Service
	events  *events.Broker
	runtime *runtime.Status
}

func NewRouter(
	projector *state.Projector,
	controlService *control.Service,
	eventBroker *events.Broker,
	runtimeStatus *runtime.Status,
) *Router {
	return &Router{
		state:   projector,
		control: controlService,
		events:  eventBroker,
		runtime: runtimeStatus,
	}
}

func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/health", r.handleHealth)
	mux.HandleFunc("/api/v2/state", r.handleState)
	mux.HandleFunc("/api/v2/capabilities", r.handleCapabilities)
	mux.HandleFunc("/api/v2/entrypoints", r.handleEntrypoints)
	mux.HandleFunc("GET /api/v2/events", r.handleEventsSSE)
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/unlock", r.handleEntrypointUnlock)
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/stream/start", r.handleEntrypointStreamStart)
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/stream/stop", r.handleEntrypointStreamStop)
	return mux
}

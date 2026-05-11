package v2

import (
	"net/http"

	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/security"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/runtime"
	"bticino-go-companion/internal/services/state"
	"bticino-go-companion/internal/services/trace"
)

type Router struct {
	auth    *auth.Store
	guard   *security.Guard
	state   *state.Projector
	control *control.Service
	events  *events.Broker
	runtime *runtime.Status
	trace   *trace.Broker
}

func NewRouter(
	authStore *auth.Store,
	guard *security.Guard,
	projector *state.Projector,
	controlService *control.Service,
	eventBroker *events.Broker,
	runtimeStatus *runtime.Status,
	traceBroker *trace.Broker,
) *Router {
	return &Router{
		auth:    authStore,
		guard:   guard,
		state:   projector,
		control: controlService,
		events:  eventBroker,
		runtime: runtimeStatus,
		trace:   traceBroker,
	}
}

func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public bootstrap and liveness endpoints.
	mux.HandleFunc("GET /api/v2/health", r.handleHealth)
	mux.HandleFunc("POST /api/v2/pair/challenge", r.handlePairChallenge)
	mux.HandleFunc("POST /api/v2/pair/claim", r.handlePairClaim)
	mux.HandleFunc("GET /api/v2/auth/status", r.handleAuthStatus)

	// Protected auth lifecycle and admin recovery endpoints.
	mux.HandleFunc("POST /api/v2/auth/rotate", r.withBearer(r.handleAuthRotate))
	mux.HandleFunc("POST /api/v2/auth/revoke", r.withBearer(r.handleAuthRevoke))
	mux.HandleFunc("POST /api/v2/admin/issue-repair-code", r.withBearer(r.handleIssueRepairCode))
	mux.HandleFunc("POST /api/v2/admin/reset-claim", r.withBearer(r.handleResetClaim))

	// Protected read endpoints.
	mux.HandleFunc("GET /api/v2/capabilities", r.withBearer(r.handleCapabilities))
	mux.HandleFunc("GET /api/v2/entrypoints", r.withBearer(r.handleEntrypoints))
	mux.HandleFunc("GET /api/v2/state", r.withBearer(r.handleState))
	mux.HandleFunc("GET /api/v2/events", r.withBearer(r.handleEventsSSE))
	mux.HandleFunc("GET /api/v2/trace/openwebnet", r.withBearer(r.handleOpenWebNetTrace))
	mux.HandleFunc("GET /api/v2/trace/openwebnet/stream", r.withBearer(r.handleOpenWebNetTraceStream))

	// Protected control endpoints.
	mux.HandleFunc("POST /api/v2/control/call/answer", r.withBearer(r.handleCallAnswer))
	mux.HandleFunc("POST /api/v2/control/call/hangup", r.withBearer(r.handleCallHangup))
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/unlock", r.withBearer(r.handleEntrypointUnlock))
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/stream/start", r.withBearer(r.handleEntrypointStreamStart))
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/stream/stop", r.withBearer(r.handleEntrypointStreamStop))
	return mux
}

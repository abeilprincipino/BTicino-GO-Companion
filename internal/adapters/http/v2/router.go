package v2

import (
	"net/http"

	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/diagnostics"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/runtime"
	"bticino-go-companion/internal/services/state"
	"bticino-go-companion/internal/services/systemcontrol"
	"bticino-go-companion/internal/services/trace"
	"bticino-go-companion/internal/services/update"
)

type Router struct {
	cfg     config.Config
	auth    *auth.Store
	state   *state.Projector
	control *control.Service
	events  *events.Broker
	runtime *runtime.Status
	trace   *trace.Broker
	system  *systemcontrol.Service
	update  *update.Manager
	diag    *diagnostics.Service
}

func NewRouter(
	cfg config.Config,
	authStore *auth.Store,
	projector *state.Projector,
	controlService *control.Service,
	eventBroker *events.Broker,
	runtimeStatus *runtime.Status,
	traceBroker *trace.Broker,
	systemControl *systemcontrol.Service,
	updateManager *update.Manager,
	diagnosticsService *diagnostics.Service,
) *Router {
	return &Router{
		cfg:     cfg,
		auth:    authStore,
		state:   projector,
		control: controlService,
		events:  eventBroker,
		runtime: runtimeStatus,
		trace:   traceBroker,
		system:  systemControl,
		update:  updateManager,
		diag:    diagnosticsService,
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
	mux.HandleFunc("GET /api/v2/voicemail/messages", r.withBearer(r.handleVoicemailMessages))
	mux.HandleFunc("GET /api/v2/voicemail/messages/{message_id}/{asset}", r.withBearer(r.handleVoicemailAsset))

	// Protected control endpoints.
	mux.HandleFunc("POST /api/v2/control/call/answer", r.withBearer(r.handleCallAnswer))
	mux.HandleFunc("POST /api/v2/control/call/hangup", r.withBearer(r.handleCallHangup))
	mux.HandleFunc("POST /api/v2/control/audio/mute", r.withBearer(r.handleAudioMute))
	mux.HandleFunc("POST /api/v2/control/audio/unmute", r.withBearer(r.handleAudioUnmute))
	mux.HandleFunc("POST /api/v2/control/voicemail/enable", r.withBearer(r.handleVoicemailEnable))
	mux.HandleFunc("POST /api/v2/control/voicemail/disable", r.withBearer(r.handleVoicemailDisable))
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/unlock", r.withBearer(r.handleEntrypointUnlock))
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/stream/start", r.withBearer(r.handleEntrypointStreamStart))
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/stream/stop", r.withBearer(r.handleEntrypointStreamStop))
	mux.HandleFunc("POST /api/v2/control/system/reboot", r.withBearer(r.handleSystemReboot))
	mux.HandleFunc("GET /api/v2/control/system/services", r.withBearer(r.handleSystemServices))
	mux.HandleFunc("GET /api/v2/control/system/services/{name}/status", r.withBearer(r.handleSystemServiceStatus))
	mux.HandleFunc("POST /api/v2/control/system/services/{name}/restart", r.withBearer(r.handleSystemServiceRestart))
	mux.HandleFunc("GET /api/v2/control/system/update/status", r.withBearer(r.handleSystemUpdateStatus))
	mux.HandleFunc("POST /api/v2/control/system/update/check", r.withBearer(r.handleSystemUpdateCheck))
	mux.HandleFunc("POST /api/v2/control/system/update/apply", r.withBearer(r.handleSystemUpdateApply))
	mux.HandleFunc("POST /api/v2/control/system/update/rollback", r.withBearer(r.handleSystemUpdateRollback))
	return mux
}

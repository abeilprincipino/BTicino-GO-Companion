package v2

import (
	"net/http"

	"bticino-go-companion/internal/observability"
	"bticino-go-companion/internal/services/runtime"
)

func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	snap := r.state.Snapshot()
	runtimeSnap := runtime.Snapshot{}
	if r.runtime != nil {
		runtimeSnap = r.runtime.Snapshot()
	}
	writeJSON(w, http.StatusOK, observability.New(snap.BootTime, runtimeSnap))
}

func (r *Router) handleState(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	snap := r.state.Snapshot()
	network := map[string]any{
		"ip":            nullableString(r.cfg.DeviceIP),
		"mac":           nullableString(r.cfg.DeviceMAC),
		"wifi_rssi":     r.cfg.DeviceWiFiRSSI,
		"wifi_strength": r.cfg.DeviceWiFiRSSI,
	}
	response := map[string]any{
		"boot_time":         snap.BootTime,
		"call_state":        snap.CallState,
		"stream_active":     snap.StreamActive,
		"active_entrypoint": stateEntrypointValue(snap.ActiveEntrypoint),
		"ringing":           snap.Ringing,
		"floor_ringing":     snap.FloorRinging,
		"last_event_type":   snap.LastEventType,
		"last_event_ts":     snap.LastEventTS,
		"entrypoints":       snap.Entrypoints,
		"device": map[string]any{
			"id":       nullableString(r.auth.DeviceID()),
			"name":     nullableString(r.cfg.DeviceModel),
			"model":    nullableString(r.cfg.DeviceModel),
			"firmware": nullableString(r.cfg.DeviceFirmware),
		},
		"diagnostics": map[string]any{
			"network": network,
		},
	}
	if snap.LastEventType == "" {
		delete(response, "last_event_type")
	}
	if snap.LastEventTS == nil {
		delete(response, "last_event_ts")
	}
	writeJSON(w, http.StatusOK, response)
}

func (r *Router) handleCapabilities(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version": "v2",
		"capabilities": []string{
			"entrypoints_v2",
			"events_v2",
			"control_entrypoints_v2",
			"control_call_v2",
			"trace_openwebnet_v2",
			"pairing_v2",
			"auth_v2",
		},
	})
}

func (r *Router) handleEntrypoints(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	snap := r.state.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"entrypoints": snap.Entrypoints})
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func stateEntrypointValue(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

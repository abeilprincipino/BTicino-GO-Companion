package v2

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/observability"
	"bticino-go-companion/internal/services/runtime"
)

type entrypointResponse struct {
	entrypoint.Model
	RTSPPath string `json:"rtsp_path,omitempty"`
	RTSPPort int    `json:"rtsp_port,omitempty"`
}

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
	network := map[string]any{}
	if r.diag != nil {
		diag := r.diag.NetworkSnapshot()
		network["ip"] = nullableString(diag.IP)
		network["netmask"] = nullableString(diag.Netmask)
		network["mac"] = nullableString(diag.MAC)
		network["wifi_strength"] = diag.WiFiStrength
		network["updated_at"] = diag.UpdatedAt
		network["stale"] = diag.Stale
	} else {
		network["ip"] = nullableString(r.cfg.DeviceIP)
		network["netmask"] = nullableString(r.cfg.DeviceNetmask)
		network["mac"] = nullableString(r.cfg.DeviceMAC)
		network["wifi_strength"] = nil
		network["stale"] = true
	}
	response := map[string]any{
		"boot_time":         snap.BootTime,
		"call_state":        snap.CallState,
		"stream_active":     snap.StreamActive,
		"active_entrypoint": stateEntrypointValue(snap.ActiveEntrypoint),
		"audio": map[string]any{
			"muted": snap.AudioMuted,
		},
		"voicemail": map[string]any{
			"enabled":                 snap.VoicemailEnabled,
			"welcome_message_enabled": snap.VoicemailWelcomeMessageEnabled,
		},
		"floor_ringing":   snap.FloorRinging,
		"last_event_type": snap.LastEventType,
		"last_event_ts":   snap.LastEventTS,
		"device": map[string]any{
			"id":       nullableString(r.auth.DeviceID()),
			"name":     nullableString(r.cfg.DeviceModel),
			"model":    nullableString(r.cfg.DeviceModel),
			"firmware": nullableString(r.cfg.DeviceFirmware),
			"hardware": nullableString(r.cfg.DeviceHardware),
		},
		"diagnostics": map[string]any{
			"network": network,
			"system": map[string]any{
				"kernel":       nullableString(r.cfg.DeviceKernel),
				"distribution": nullableString(r.cfg.DeviceDistribution),
			},
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
	capabilities := []string{
		"entrypoints_v2",
		"events_v2",
		"control_entrypoints_v2",
		"control_call_v2",
		"trace_openwebnet_v2",
		"pairing_v2",
		"auth_v2",
	}
	if r.cfg.ExposeMuteControl {
		capabilities = append(capabilities, "control_audio_v2")
	}
	if r.cfg.ExposeVoicemailToggle && !strings.EqualFold(strings.TrimSpace(r.cfg.DeviceModel), "C100X") {
		capabilities = append(capabilities, "control_voicemail_v2", "voicemail_messages_v2")
	}
	if r.cfg.SystemRebootEnabled {
		capabilities = append(capabilities, "control_system_reboot_v2")
	}
	if len(r.cfg.SystemServices) > 0 {
		capabilities = append(capabilities, "control_system_services_v2")
	}
	if r.cfg.SystemUpdateEnabled && r.cfg.SystemUpdateExposed {
		capabilities = append(capabilities, "control_system_update_v2")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version":  "v2",
		"capabilities": capabilities,
		"system_control": map[string]any{
			"reboot_enabled": r.cfg.SystemRebootEnabled,
			"services":       r.cfg.SystemServices,
			"update":         r.systemUpdateSnapshot(),
		},
	})
}

func (r *Router) handleEntrypoints(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	snap := r.state.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"entrypoints": r.entrypointResponses(snap.Entrypoints)})
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

func (r *Router) entrypointResponses(entrypoints []entrypoint.Model) []entrypointResponse {
	paths := entrypoint.RTSPPathByEntrypointID(entrypoints)
	port := rtspPort(r.cfg.MediaRTSPAddress)
	out := make([]entrypointResponse, 0, len(entrypoints))
	for _, ep := range entrypoints {
		row := entrypointResponse{Model: ep}
		if ep.HasStream {
			row.RTSPPath = paths[strings.TrimSpace(ep.ID)]
			row.RTSPPort = port
		}
		out = append(out, row)
	}
	return out
}

func rtspPort(address string) int {
	const fallback = 8554
	raw := strings.TrimSpace(address)
	if raw == "" {
		return fallback
	}
	if strings.HasPrefix(raw, ":") {
		port, err := strconv.Atoi(strings.TrimPrefix(raw, ":"))
		if err == nil && port > 0 && port <= 65535 {
			return port
		}
		return fallback
	}
	_, portString, err := net.SplitHostPort(raw)
	if err != nil {
		return fallback
	}
	port, err := strconv.Atoi(strings.TrimSpace(portString))
	if err != nil || port <= 0 || port > 65535 {
		return fallback
	}
	return port
}

func (r *Router) systemUpdateSnapshot() map[string]any {
	out := map[string]any{
		"enabled":        r.cfg.SystemUpdateEnabled,
		"exposed":        r.cfg.SystemUpdateExposed,
		"allow_apply":    r.cfg.SystemUpdateAllowApply,
		"allow_rollback": r.cfg.SystemUpdateAllowRollback,
	}
	if r.update != nil {
		out["status"] = r.update.Status()
	}
	return out
}

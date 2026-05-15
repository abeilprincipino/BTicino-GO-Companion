package v2

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"bticino-go-companion/internal/services/systemcontrol"
)

func (r *Router) handleSystemReboot(w http.ResponseWriter, req *http.Request) {
	if r.system == nil {
		writeError(w, http.StatusServiceUnavailable, "system_control_unavailable", "system control is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
	defer cancel()
	if err := r.system.Reboot(ctx); err != nil {
		handleSystemControlError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "action": "system_reboot", "rebooting": true})
}

func (r *Router) handleSystemServices(w http.ResponseWriter, req *http.Request) {
	if r.system == nil {
		writeError(w, http.StatusServiceUnavailable, "system_control_unavailable", "system control is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reboot_enabled": r.system.RebootEnabled(),
		"services":       r.system.ServiceConfigs(),
	})
}

func (r *Router) handleSystemServiceStatus(w http.ResponseWriter, req *http.Request) {
	if r.system == nil {
		writeError(w, http.StatusServiceUnavailable, "system_control_unavailable", "system control is unavailable")
		return
	}
	serviceName := strings.TrimSpace(req.PathValue("name"))
	ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
	defer cancel()
	status, err := r.system.ServiceStatus(ctx, serviceName)
	if err != nil {
		handleSystemControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": status,
	})
}

func (r *Router) handleSystemServiceRestart(w http.ResponseWriter, req *http.Request) {
	if r.system == nil {
		writeError(w, http.StatusServiceUnavailable, "system_control_unavailable", "system control is unavailable")
		return
	}
	serviceName := strings.TrimSpace(req.PathValue("name"))
	ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
	defer cancel()
	if err := r.system.RestartService(ctx, serviceName); err != nil {
		handleSystemControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"action":  "service_restart",
		"service": strings.ToLower(serviceName),
	})
}

func handleSystemControlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, systemcontrol.ErrSystemControlUnavailable):
		writeError(w, http.StatusServiceUnavailable, "system_control_unavailable", err.Error())
	case errors.Is(err, systemcontrol.ErrRebootDisabled):
		writeError(w, http.StatusConflict, "reboot_disabled", err.Error())
	case errors.Is(err, systemcontrol.ErrServiceDisabled):
		writeError(w, http.StatusConflict, "service_disabled", err.Error())
	case errors.Is(err, systemcontrol.ErrServiceNameInvalid):
		writeError(w, http.StatusBadRequest, "service_name_invalid", err.Error())
	case errors.Is(err, systemcontrol.ErrServiceNotExposed):
		writeError(w, http.StatusNotFound, "service_not_exposed", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "system_control_failed", err.Error())
	}
}

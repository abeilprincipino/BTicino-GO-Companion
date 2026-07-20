package api

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/system"
	"context"
	"errors"
	"net/http"
	"strconv"
)

var (
	ErrEntrypointNotFound = errors.New("entrypoint not found")
	ErrCapabilityDisabled = errors.New("entrypoint capability is disabled")
)

func (s *Server) unlockEntrypoint(w http.ResponseWriter, r *http.Request) {
	entrypoint, err := s.entrypoint(r.PathValue("id"))
	if err != nil {
		writeEntrypointError(w, err)
		return
	}
	if !entrypoint.Capabilities.Unlock {
		writeEntrypointError(w, ErrCapabilityDisabled)
		return
	}
	if s.entrypoints == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "unlock control is unavailable")
		return
	}
	if err := s.entrypoints.Unlock(r.Context(), core.EntrypointID(entrypoint.ID)); err != nil {
		s.logger.ErrorContext(r.Context(), "entrypoint unlock failed", "entrypoint_id", entrypoint.ID, "error", err)
		writeCommandError(w, err)
		return
	}
	s.logger.InfoContext(r.Context(), "entrypoint unlocked", "entrypoint_id", entrypoint.ID)
	s.BroadcastState()
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}

func (s *Server) muteAudio(w http.ResponseWriter, r *http.Request) {
	if s.audio == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "audio control is unavailable")
		return
	}
	if err := s.audio.Mute(r.Context()); err != nil {
		writeCommandError(w, err)
		return
	}
	s.BroadcastState()
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}

func (s *Server) unmuteAudio(w http.ResponseWriter, r *http.Request) {
	if s.audio == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "audio control is unavailable")
		return
	}
	if err := s.audio.Unmute(r.Context()); err != nil {
		writeCommandError(w, err)
		return
	}
	s.BroadcastState()
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}

func (s *Server) enableVoicemail(w http.ResponseWriter, r *http.Request) { s.setVoicemail(w, r, true) }
func (s *Server) disableVoicemail(w http.ResponseWriter, r *http.Request) {
	s.setVoicemail(w, r, false)
}

func (s *Server) setVoicemail(w http.ResponseWriter, r *http.Request, enabled bool) {
	if s.voicemail == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "voicemail control is unavailable")
		return
	}
	var err error
	if enabled {
		err = s.voicemail.Enable(r.Context())
	} else {
		err = s.voicemail.Disable(r.Context())
	}
	if err != nil {
		writeCommandError(w, err)
		return
	}
	s.BroadcastState()
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}

func (s *Server) voicemailRefresh(w http.ResponseWriter, r *http.Request) {
	if s.refreshVoicemail == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "voicemail refresh is unavailable")
		return
	}
	available, err := s.refreshVoicemail(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "voicemail refresh failed", "error", err)
		writeCommandError(w, err)
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"available": available, "state": s.currentPayload()})
}

func (s *Server) entrypoint(id string) (config.Entrypoint, error) {
	if s.config != nil {
		for _, entrypoint := range s.config.Snapshot().Companion.Entrypoints {
			if entrypoint.ID == id {
				return entrypoint, nil
			}
		}
	}
	return config.Entrypoint{}, ErrEntrypointNotFound
}

func writeEntrypointError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrEntrypointNotFound):
		writeError(w, http.StatusNotFound, "entrypoint_not_found", "entrypoint is not configured")
	case errors.Is(err, ErrCapabilityDisabled):
		writeError(w, http.StatusConflict, "capability_disabled", "entrypoint unlock is disabled")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "entrypoint request failed")
	}
}

func writeCommandError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	code := "command_failed"

	switch {
	case errors.Is(err, system.ErrRuntimeUnavailable), errors.Is(err, system.ErrUpdateUnavailable), errors.Is(err, system.ErrServiceNotAllowed), errors.Is(err, media.ErrSnapshotUnavailable):
		status = http.StatusServiceUnavailable
		code = "unavailable"
	}

	writeError(w, status, code, "command could not be completed")
}

func (s *Server) systemReboot(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil || !s.runtime.RebootAvailable() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "runtime control is unavailable")
		return
	}
	if s.config == nil || !s.config.Snapshot().System.RebootEnabled {
		writeError(w, http.StatusConflict, "reboot_disabled", "reboot control is disabled")
		return
	}

	writeOK(w, http.StatusOK, nil)
	s.logger.InfoContext(r.Context(), "system reboot requested")
	go func() {
		if err := s.runtime.Reboot(context.WithoutCancel(r.Context())); err != nil {
			s.logger.Error("system reboot failed", "error", err)
		}
	}()
}

func (s *Server) systemServiceRestart(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "runtime control is unavailable")
		return
	}
	serviceName := r.PathValue("name")
	if s.config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "configuration is unavailable")
		return
	}
	service, ok := s.config.Snapshot().System.Services[serviceName]
	if !ok || !service.Enabled || !service.Exposed {
		writeError(w, http.StatusConflict, "service_disabled", "service restart is disabled")
		return
	}

	writeOK(w, http.StatusAccepted, nil)
	s.logger.InfoContext(r.Context(), "service restart requested", "service_name", serviceName)
	go func() {
		if err := s.runtime.Restart(context.WithoutCancel(r.Context()), serviceName); err != nil {
			s.logger.Error("service restart failed", "service_name", serviceName, "error", err)
		}
	}()
}

func (s *Server) systemServiceStatus(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "runtime control is unavailable")
		return
	}

	status, err := s.runtime.Status(r.Context(), r.PathValue("name"))
	if err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) systemUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "update control is unavailable")
		return
	}

	status, err := s.update.Status(r.Context())
	if err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) systemUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "update control is unavailable")
		return
	}

	status, err := s.update.Check(r.Context())
	if err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) systemUpdateStage(w http.ResponseWriter, r *http.Request) {
	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "update control is unavailable")
		return
	}

	status, err := s.update.Stage(r.Context())
	if err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) systemUpdateInstall(w http.ResponseWriter, r *http.Request) {
	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "update control is unavailable")
		return
	}

	status, err := s.update.Install(r.Context())
	if err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) snapshotLatest(w http.ResponseWriter, r *http.Request) {
	if s.snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "snapshot is unavailable")
		return
	}

	image, err := s.snapshot.Latest(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, media.ErrSnapshotNotFound) {
			writeError(w, http.StatusNotFound, "snapshot_not_found", "snapshot is not available yet")
			return
		}
		writeCommandError(w, err)
		return
	}

	writeJPEG(w, image)
}

func writeJPEG(w http.ResponseWriter, image []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(image)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image)
}

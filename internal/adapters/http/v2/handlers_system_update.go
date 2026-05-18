package v2

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"bticino-go-companion/internal/services/update"
)

func (r *Router) handleSystemUpdateStatus(w http.ResponseWriter, req *http.Request) {
	if !r.cfg.SystemUpdateEnabled || !r.cfg.SystemUpdateExposed {
		writeError(w, http.StatusNotFound, "update_not_exposed", "system update control is not exposed")
		return
	}
	if r.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update_unavailable", "update service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, r.update.Status())
}

func decodeOptionalUpdateJSONBody(req *http.Request, dst any) (bool, error) {
	if req.Body == nil || req.ContentLength == 0 {
		return false, nil
	}
	defer req.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Router) handleSystemUpdateCheck(w http.ResponseWriter, req *http.Request) {
	if !r.cfg.SystemUpdateEnabled || !r.cfg.SystemUpdateExposed {
		writeError(w, http.StatusNotFound, "update_not_exposed", "system update control is not exposed")
		return
	}
	if r.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update_unavailable", "update service unavailable")
		return
	}
	var body struct {
		AvailableVersion string `json:"available_version"`
		ArtifactPath     string `json:"artifact_path"`
		SHA256           string `json:"sha256"`
	}
	override := (*update.Artifact)(nil)
	if hasBody, err := decodeOptionalUpdateJSONBody(req, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json body")
		return
	} else if hasBody {
		version := strings.TrimSpace(body.AvailableVersion)
		path := strings.TrimSpace(body.ArtifactPath)
		sha := strings.TrimSpace(strings.ToLower(body.SHA256))
		if version != "" || path != "" || sha != "" {
			override = &update.Artifact{Version: version, Path: path, SHA256: sha}
		}
	}
	status, err := r.update.Check(override)
	if err != nil {
		writeError(w, http.StatusBadRequest, "update_check_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (r *Router) handleSystemUpdateApply(w http.ResponseWriter, req *http.Request) {
	if !r.cfg.SystemUpdateEnabled || !r.cfg.SystemUpdateExposed {
		writeError(w, http.StatusNotFound, "update_not_exposed", "system update control is not exposed")
		return
	}
	if !r.cfg.SystemUpdateAllowApply {
		writeError(w, http.StatusConflict, "update_apply_disabled", "update apply is disabled")
		return
	}
	if r.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update_unavailable", "update service unavailable")
		return
	}
	var body struct {
		Version      string `json:"version"`
		ArtifactPath string `json:"artifact_path"`
		SHA256       string `json:"sha256"`
	}
	override := (*update.Artifact)(nil)
	if hasBody, err := decodeOptionalUpdateJSONBody(req, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json body")
		return
	} else if hasBody {
		override = &update.Artifact{
			Version: strings.TrimSpace(body.Version),
			Path:    strings.TrimSpace(body.ArtifactPath),
			SHA256:  strings.TrimSpace(strings.ToLower(body.SHA256)),
		}
	}
	status, err := r.update.Apply(override)
	if err != nil {
		writeError(w, http.StatusBadGateway, "update_apply_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func (r *Router) handleSystemUpdateRollback(w http.ResponseWriter, req *http.Request) {
	if !r.cfg.SystemUpdateEnabled || !r.cfg.SystemUpdateExposed {
		writeError(w, http.StatusNotFound, "update_not_exposed", "system update control is not exposed")
		return
	}
	if !r.cfg.SystemUpdateAllowRollback {
		writeError(w, http.StatusConflict, "update_rollback_disabled", "update rollback is disabled")
		return
	}
	if r.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update_unavailable", "update service unavailable")
		return
	}
	status, err := r.update.Rollback()
	if err != nil {
		writeError(w, http.StatusBadGateway, "update_rollback_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

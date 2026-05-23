package v2

import (
	"context"
	"errors"
	"net/http"
	"time"

	rtspadapter "bticino-go-companion/internal/adapters/rtsp"
	"bticino-go-companion/internal/services/media"
	"bticino-go-companion/internal/services/snapshot"
)

const snapshotRequestTimeout = 20 * time.Second

func (r *Router) handleEntrypointSnapshotCapture(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if r.snap == nil {
		writeError(w, http.StatusServiceUnavailable, "snapshot_unavailable", "snapshot service unavailable")
		return
	}
	entrypointID := req.PathValue("id")
	ctx, cancel := context.WithTimeout(req.Context(), snapshotRequestTimeout)
	defer cancel()

	image, err := r.snap.Capture(ctx, entrypointID)
	if err != nil {
		handleSnapshotError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(image)
}

func (r *Router) handleEntrypointSnapshotLatest(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if r.snap == nil {
		writeError(w, http.StatusServiceUnavailable, "snapshot_unavailable", "snapshot service unavailable")
		return
	}
	entrypointID := req.PathValue("id")
	assetPath, err := r.snap.Latest(entrypointID)
	if err != nil {
		handleSnapshotError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, req, assetPath)
}

func handleSnapshotError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, snapshot.ErrEntrypointNotFound):
		writeError(w, http.StatusNotFound, "entrypoint_not_found", err.Error())
	case errors.Is(err, snapshot.ErrCapabilityNotEnabled):
		writeError(w, http.StatusConflict, "capability_disabled", err.Error())
	case errors.Is(err, snapshot.ErrSnapshotBusy), errors.Is(err, media.ErrEntrypointSwitchBlocked), errors.Is(err, snapshot.ErrActiveEntrypointBlocked), errors.Is(err, rtspadapter.ErrSnapshotMirrorBusy):
		writeError(w, http.StatusConflict, "snapshot_busy", err.Error())
	case errors.Is(err, snapshot.ErrSnapshotUnavailable):
		writeError(w, http.StatusServiceUnavailable, "snapshot_unavailable", err.Error())
	case errors.Is(err, snapshot.ErrSnapshotTimeout):
		writeError(w, http.StatusGatewayTimeout, "snapshot_timeout", err.Error())
	case errors.Is(err, snapshot.ErrSnapshotNotFound):
		writeError(w, http.StatusNotFound, "snapshot_not_found", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "snapshot_failed", err.Error())
	}
}

package v2

import (
	"errors"
	"net/http"
	"strings"

	"bticino-go-companion/internal/services/webrtc"
)

type webRTCOfferRequest struct {
	SessionID    string `json:"session_id"`
	EntrypointID string `json:"entrypoint_id"`
	OfferSDP     string `json:"offer_sdp"`
}

type webRTCCandidateRequest struct {
	SessionID string           `json:"session_id"`
	Candidate webrtc.Candidate `json:"candidate"`
}

type webRTCCloseRequest struct {
	SessionID string `json:"session_id"`
}

func (r *Router) handleWebRTCOffer(w http.ResponseWriter, req *http.Request) {
	if r.webrtc == nil {
		writeError(w, http.StatusServiceUnavailable, "webrtc_unavailable", "webrtc service unavailable")
		return
	}

	var payload webRTCOfferRequest
	if err := decodeRequiredJSONBody(req, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	ctx, cancel := contextWithTimeout(req)
	defer cancel()

	result, err := r.webrtc.HandleOffer(ctx, payload.SessionID, payload.EntrypointID, payload.OfferSDP)
	if err != nil {
		handleWebRTCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"session_id":    result.SessionID,
		"entrypoint_id": result.EntrypointID,
		"answer_sdp":    result.AnswerSDP,
		"candidates":    result.Candidates,
	})
}

func (r *Router) handleWebRTCCandidate(w http.ResponseWriter, req *http.Request) {
	if r.webrtc == nil {
		writeError(w, http.StatusServiceUnavailable, "webrtc_unavailable", "webrtc service unavailable")
		return
	}

	var payload webRTCCandidateRequest
	if err := decodeRequiredJSONBody(req, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(payload.Candidate.Candidate) == "" {
		writeError(w, http.StatusBadRequest, "invalid_candidate", "candidate is required")
		return
	}
	if err := r.webrtc.AddCandidate(payload.SessionID, payload.Candidate); err != nil {
		handleWebRTCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": strings.TrimSpace(payload.SessionID),
		"action":     "candidate",
	})
}

func (r *Router) handleWebRTCClose(w http.ResponseWriter, req *http.Request) {
	if r.webrtc == nil {
		writeError(w, http.StatusServiceUnavailable, "webrtc_unavailable", "webrtc service unavailable")
		return
	}

	var payload webRTCCloseRequest
	if err := decodeRequiredJSONBody(req, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := r.webrtc.Close(payload.SessionID); err != nil {
		handleWebRTCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": strings.TrimSpace(payload.SessionID),
		"action":     "close",
	})
}

func handleWebRTCError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, webrtc.ErrSessionIDRequired):
		writeError(w, http.StatusBadRequest, "invalid_session_id", err.Error())
	case errors.Is(err, webrtc.ErrEntrypointRequired):
		writeError(w, http.StatusBadRequest, "invalid_entrypoint_id", err.Error())
	case errors.Is(err, webrtc.ErrOfferRequired):
		writeError(w, http.StatusBadRequest, "invalid_offer_sdp", err.Error())
	case errors.Is(err, webrtc.ErrCandidateRequired):
		writeError(w, http.StatusBadRequest, "invalid_candidate", err.Error())
	case errors.Is(err, webrtc.ErrSessionExists):
		writeError(w, http.StatusConflict, "session_exists", err.Error())
	case errors.Is(err, webrtc.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "session_not_found", err.Error())
	case errors.Is(err, webrtc.ErrEntrypointNotFound):
		writeError(w, http.StatusNotFound, "entrypoint_not_found", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "webrtc_failed", err.Error())
	}
}

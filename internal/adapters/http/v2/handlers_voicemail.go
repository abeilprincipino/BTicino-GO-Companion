package v2

import (
	"errors"
	"net/http"

	"bticino-go-companion/internal/system"
)

func (r *Router) handleVoicemailMessages(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	messages, err := system.ListVoicemailMessages(r.cfg.VoicemailMessagesDir)
	if err != nil {
		writeError(w, http.StatusBadGateway, "voicemail_list_failed", "could not list voicemail messages")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": messages,
		"count":    len(messages),
	})
}

func (r *Router) handleVoicemailAsset(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	messageID := req.PathValue("message_id")
	asset := req.PathValue("asset")
	assetPath, contentType, err := system.VoicemailAssetPath(r.cfg.VoicemailMessagesDir, messageID, asset)
	if err != nil {
		if errors.Is(err, system.ErrVoicemailPathTraversal) || errors.Is(err, system.ErrVoicemailInvalidAsset) {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid voicemail asset request")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid voicemail request")
		return
	}

	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, req, assetPath)
}

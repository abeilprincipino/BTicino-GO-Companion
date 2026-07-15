package api

import (
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/system"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

func (s *Server) command(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.commands == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "command handler is unavailable")
			return
		}

		payload, err := readBody(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
			return
		}

		result, err := s.commands.HandleCommand(r, Command{Action: action, Payload: payload})
		if err != nil {
			writeCommandError(w, err)
			return
		}

		writeOK(w, http.StatusOK, map[string]any{"result": result})
	}
}

func (s *Server) entrypointCommand(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.command("entrypoints."+r.PathValue("id")+"."+verb)(w, r)
	}
}

func readBody(r *http.Request) (json.RawMessage, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return nil, err
	}

	if len(data) > maxJSONBody {
		return nil, errors.New("request body is too large")
	}

	if len(data) == 0 {
		return json.RawMessage("{}"), nil
	}

	return json.RawMessage(data), nil
}

func writeCommandError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	code := "command_failed"

	switch {
	case errors.Is(err, ErrNotImplemented):
		status = http.StatusNotImplemented
		code = "not_implemented"
	case errors.Is(err, system.ErrRuntimeUnavailable), errors.Is(err, system.ErrUpdateUnavailable), errors.Is(err, system.ErrServiceNotAllowed), errors.Is(err, media.ErrSnapshotUnavailable), errors.Is(err, media.ErrSnapshotNoVideo):
		status = http.StatusServiceUnavailable
		code = "unavailable"
	}

	writeError(w, status, code, err.Error())
}

func (s *Server) webrtcOffer(w http.ResponseWriter, r *http.Request) {
	if s.webrtc == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "webrtc control is unavailable")
		return
	}

	var req struct {
		Source    media.Source             `json:"source"`
		SessionID media.SessionID          `json:"session_id"`
		Offer     media.SessionDescription `json:"offer"`
	}

	if !decodeJSON(w, r, &req) {
		return
	}

	answer, err := s.webrtc.Offer(req.Source, req.SessionID, req.Offer)
	if err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, map[string]any{"answer": answer})
}

func (s *Server) webrtcCandidate(w http.ResponseWriter, r *http.Request) {
	if s.webrtc == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "webrtc control is unavailable")
		return
	}

	var req struct {
		SessionID media.SessionID    `json:"session_id"`
		Candidate media.ICECandidate `json:"candidate"`
	}

	if !decodeJSON(w, r, &req) {
		return
	}

	if err := s.webrtc.AddCandidate(req.SessionID, req.Candidate); err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, nil)
}

func (s *Server) webrtcClose(w http.ResponseWriter, r *http.Request) {
	if s.webrtc == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "webrtc control is unavailable")
		return
	}

	var req struct {
		SessionID media.SessionID `json:"session_id"`
	}

	if !decodeJSON(w, r, &req) {
		return
	}

	if err := s.webrtc.Close(req.SessionID); err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, nil)
}

func (s *Server) systemReboot(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "runtime control is unavailable")
		return
	}

	if err := s.runtime.Reboot(r.Context()); err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, nil)
}

func (s *Server) systemServiceRestart(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "runtime control is unavailable")
		return
	}

	if err := s.runtime.Restart(r.Context(), r.PathValue("name")); err != nil {
		writeCommandError(w, err)
		return
	}

	writeOK(w, http.StatusOK, nil)
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

func (s *Server) snapshotLatest(w http.ResponseWriter, r *http.Request) {
	if s.snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "snapshot is unavailable")
		return
	}

	image, err := s.snapshot.Capture(r.Context(), core.EntrypointID(r.PathValue("id")))
	if err != nil {
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

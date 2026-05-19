package v2

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"bticino-go-companion/internal/auth"
)

func (r *Router) withBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r.auth == nil {
			writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "auth service unavailable")
			return
		}

		token := bearerToken(req.Header.Get("Authorization"))
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		if err := r.auth.ValidateBearer(token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}

		next(w, req)
	}
}

func (r *Router) handlePairChallenge(w http.ResponseWriter, _ *http.Request) {
	if r.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "auth service unavailable")
		return
	}

	ch, err := r.auth.StartChallenge()
	if err != nil {
		if errors.Is(err, auth.ErrAlreadyClaimed) {
			writeError(w, http.StatusConflict, "already_claimed", "device already claimed")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "could not start challenge")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"challenge_id": ch.ID,
		"nonce":        ch.Nonce,
		"expires_at":   ch.ExpiresAt.Format(time.RFC3339),
		"algorithm":    "plain-v1",
	})
}

func (r *Router) handlePairClaim(w http.ResponseWriter, req *http.Request) {
	if r.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "auth service unavailable")
		return
	}

	var body auth.ClaimRequest
	if err := decodeRequiredJSONBody(req, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json body")
		return
	}

	token, keyID, err := r.auth.Claim(body)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrAlreadyClaimed):
			writeError(w, http.StatusConflict, "already_claimed", "device already claimed")
		case errors.Is(err, auth.ErrInvalidClaimCode):
			writeError(w, http.StatusUnauthorized, "invalid_claim_code", "claim code rejected")
		case errors.Is(err, auth.ErrInvalidChallenge), errors.Is(err, auth.ErrChallengeExpired):
			writeError(w, http.StatusUnauthorized, "invalid_challenge", "challenge is invalid or expired")
		default:
			writeError(w, http.StatusInternalServerError, "internal", "claim failed")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"needs_claim":  false,
		"token_type":   "Bearer",
		"access_token": token,
		"key_id":       keyID,
	})
}

func (r *Router) handleAuthStatus(w http.ResponseWriter, req *http.Request) {
	if r.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "auth service unavailable")
		return
	}
	if !r.auth.NeedsClaim() {
		token := bearerToken(req.Header.Get("Authorization"))
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		if err := r.auth.ValidateBearer(token); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"needs_claim": r.auth.NeedsClaim(),
		"key_id":      r.auth.CurrentKeyID(),
	})
}

func (r *Router) handleAuthRotate(w http.ResponseWriter, _ *http.Request) {
	token, keyID, prevKeyID, err := r.auth.Rotate()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "rotate failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token_type":      "Bearer",
		"access_token":    token,
		"key_id":          keyID,
		"replaced_key_id": prevKeyID,
	})
}

func (r *Router) handleAuthRevoke(w http.ResponseWriter, req *http.Request) {
	var body struct {
		KeyID string `json:"key_id"`
	}
	if err := decodeRequiredJSONBody(req, &body); err != nil || strings.TrimSpace(body.KeyID) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "key_id is required")
		return
	}

	token, keyID, err := r.auth.RevokeAndReplace(strings.TrimSpace(body.KeyID))
	if err != nil {
		if errors.Is(err, auth.ErrKeyNotFound) {
			writeError(w, http.StatusNotFound, "key_not_found", "key id not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "revoke failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"revoked_key_id": body.KeyID,
		"token_type":     "Bearer",
		"access_token":   token,
		"key_id":         keyID,
	})
}

func (r *Router) handleIssueRepairCode(w http.ResponseWriter, _ *http.Request) {
	code, expiresAt, err := r.auth.IssueRepairCode(10 * time.Minute)
	if err != nil {
		if errors.Is(err, auth.ErrRepairNotAllowed) {
			writeError(w, http.StatusConflict, "repair_not_allowed", "repair flow requires a claimed device")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "could not issue repair code")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"repair_code": code,
		"expires_at":  expiresAt.Format(time.RFC3339),
		"ttl_s":       int((10 * time.Minute).Seconds()),
	})
}

func (r *Router) handleResetClaim(w http.ResponseWriter, req *http.Request) {
	var body struct {
		RepairCode string `json:"repair_code"`
	}
	if err := decodeRequiredJSONBody(req, &body); err != nil || strings.TrimSpace(body.RepairCode) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "repair_code is required")
		return
	}

	claimCode, err := r.auth.ResetClaim(body.RepairCode)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrRepairNotAllowed):
			writeError(w, http.StatusConflict, "repair_not_allowed", "device is not currently claimed")
		case errors.Is(err, auth.ErrRepairCodeExpired):
			writeError(w, http.StatusUnauthorized, "repair_code_expired", "repair code has expired")
		case errors.Is(err, auth.ErrInvalidRepairCode):
			writeError(w, http.StatusUnauthorized, "invalid_repair_code", "repair code is invalid")
		default:
			writeError(w, http.StatusBadGateway, "reset_claim_failed", "could not reset claim state")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"needs_claim": true,
		"claim_code":  claimCode,
	})
}

func decodeRequiredJSONBody(req *http.Request, dst any) error {
	if req == nil || req.Body == nil {
		return errors.New("empty json body")
	}
	dec := json.NewDecoder(req.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty json body")
		}
		return err
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(trailing) > 0 {
		return errors.New("multiple json values")
	}
	return nil
}

func bearerToken(header string) string {
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

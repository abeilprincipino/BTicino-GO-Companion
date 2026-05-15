package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bticino-go-companion/internal/config"
)

const (
	defaultChallengeTTL    = 5 * time.Minute
	defaultRepairCodeTTL   = 10 * time.Minute
	defaultAccessTokenTTL  = 60 * time.Minute
	defaultRefreshTokenTTL = 30 * 24 * time.Hour
)

var (
	ErrAlreadyClaimed      = errors.New("device already claimed")
	ErrInvalidClaimCode    = errors.New("invalid claim code")
	ErrInvalidChallenge    = errors.New("invalid challenge")
	ErrChallengeExpired    = errors.New("challenge expired")
	ErrInvalidCredential   = errors.New("invalid credential")
	ErrTokenExpired        = errors.New("token expired")
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	ErrRefreshTokenExpired = errors.New("refresh token expired")
	ErrKeyNotFound         = errors.New("key not found")
	ErrRepairNotAllowed    = errors.New("repair flow is not allowed")
	ErrInvalidRepairCode   = errors.New("invalid repair code")
	ErrRepairCodeExpired   = errors.New("repair code expired")
)

type Challenge struct {
	ID        string    `json:"id"`
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ClaimRequest struct {
	ChallengeID string `json:"challenge_id"`
	Nonce       string `json:"nonce"`
	ClaimCode   string `json:"claim_code"`
}

type persistedState struct {
	DeviceID              string    `json:"device_id"`
	Claimed               bool      `json:"claimed"`
	ClaimCode             string    `json:"claim_code"`
	BearerToken           string    `json:"bearer_token"`
	KeyID                 string    `json:"key_id"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at,omitempty"`
	RefreshToken          string    `json:"refresh_token,omitempty"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at,omitempty"`
}

type Store struct {
	mu                sync.RWMutex
	path              string
	preferredDeviceID string
	state             persistedState
	challenges        map[string]Challenge
	repairCode        repairCodeState
}

type repairCodeState struct {
	Value     string
	ExpiresAt time.Time
}

func NewStore(path string, initialClaimCode string, deviceModel string, deviceMAC string) (*Store, error) {
	preferredDeviceID := deriveDeviceID(deviceModel, deviceMAC)
	if preferredDeviceID == "" {
		return nil, errors.New("device identity is required")
	}

	s := &Store{
		path:              strings.TrimSpace(path),
		preferredDeviceID: preferredDeviceID,
		challenges:        map[string]Challenge{},
	}
	if s.path == "" {
		return nil, errors.New("auth state path is required")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, fmt.Errorf("create auth state dir: %w", err)
	}
	if err := s.load(initialClaimCode); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) DeviceID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.DeviceID
}

func (s *Store) ClaimCode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.ClaimCode
}

func (s *Store) NeedsClaim() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.state.Claimed
}

func (s *Store) ValidateBearer(token string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.hasClaimedBearerLocked() {
		return ErrInvalidCredential
	}
	if !s.state.AccessTokenExpiresAt.IsZero() && time.Now().After(s.state.AccessTokenExpiresAt) {
		return ErrTokenExpired
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(s.state.BearerToken)) != 1 {
		return ErrInvalidCredential
	}
	return nil
}

func (s *Store) CurrentKeyID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.KeyID
}

func (s *Store) AccessTokenExpiresAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.AccessTokenExpiresAt
}

func (s *Store) RefreshTokenExpiresAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.RefreshTokenExpiresAt
}

func (s *Store) RefreshToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.RefreshToken
}

func (s *Store) StartChallenge() (Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.Claimed {
		return Challenge{}, ErrAlreadyClaimed
	}
	s.pruneExpiredChallengesLocked()

	ch := Challenge{
		ID:        randHex(12),
		Nonce:     randHex(16),
		ExpiresAt: time.Now().Add(defaultChallengeTTL),
	}
	s.challenges[ch.ID] = ch
	return ch, nil
}

func (s *Store) Claim(req ClaimRequest) (token string, keyID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.Claimed {
		return "", "", ErrAlreadyClaimed
	}
	ch, ok := s.challenges[strings.TrimSpace(req.ChallengeID)]
	if !ok {
		return "", "", ErrInvalidChallenge
	}
	delete(s.challenges, strings.TrimSpace(req.ChallengeID))
	if time.Now().After(ch.ExpiresAt) {
		return "", "", ErrChallengeExpired
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Nonce)), []byte(ch.Nonce)) != 1 {
		return "", "", ErrInvalidChallenge
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.ClaimCode)), []byte(s.state.ClaimCode)) != 1 {
		return "", "", ErrInvalidClaimCode
	}

	s.rotateBearerLocked()
	s.rotateRefreshLocked()
	s.state.Claimed = true
	if err := s.persistLocked(); err != nil {
		return "", "", err
	}
	return s.state.BearerToken, s.state.KeyID, nil
}

func (s *Store) Refresh(refreshToken string) (
	accessToken string,
	keyID string,
	accessExpiresAt time.Time,
	newRefreshToken string,
	refreshExpiresAt time.Time,
	err error,
) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.state.Claimed {
		return "", "", time.Time{}, "", time.Time{}, ErrInvalidCredential
	}
	if strings.TrimSpace(refreshToken) == "" || strings.TrimSpace(s.state.RefreshToken) == "" {
		return "", "", time.Time{}, "", time.Time{}, ErrInvalidRefreshToken
	}
	if subtle.ConstantTimeCompare(
		[]byte(strings.TrimSpace(refreshToken)),
		[]byte(strings.TrimSpace(s.state.RefreshToken)),
	) != 1 {
		return "", "", time.Time{}, "", time.Time{}, ErrInvalidRefreshToken
	}
	if !s.state.RefreshTokenExpiresAt.IsZero() && time.Now().After(s.state.RefreshTokenExpiresAt) {
		return "", "", time.Time{}, "", time.Time{}, ErrRefreshTokenExpired
	}

	s.rotateBearerLocked()
	s.rotateRefreshLocked()
	if err := s.persistLocked(); err != nil {
		return "", "", time.Time{}, "", time.Time{}, err
	}

	return s.state.BearerToken, s.state.KeyID, s.state.AccessTokenExpiresAt, s.state.RefreshToken, s.state.RefreshTokenExpiresAt, nil
}

func (s *Store) Rotate() (token string, keyID string, prevKeyID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasClaimedBearerLocked() {
		return "", "", "", ErrInvalidCredential
	}
	prevKeyID = s.state.KeyID
	s.rotateBearerLocked()
	s.rotateRefreshLocked()
	if err := s.persistLocked(); err != nil {
		return "", "", "", err
	}
	return s.state.BearerToken, s.state.KeyID, prevKeyID, nil
}

func (s *Store) RevokeAndReplace(keyID string) (token string, newKeyID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasClaimedBearerLocked() {
		return "", "", ErrInvalidCredential
	}
	if strings.TrimSpace(keyID) != s.state.KeyID {
		return "", "", ErrKeyNotFound
	}
	s.rotateBearerLocked()
	s.rotateRefreshLocked()
	if err := s.persistLocked(); err != nil {
		return "", "", err
	}
	return s.state.BearerToken, s.state.KeyID, nil
}

func (s *Store) IssueRepairCode(ttl time.Duration) (code string, expiresAt time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Claimed {
		return "", time.Time{}, ErrRepairNotAllowed
	}
	if ttl <= 0 {
		ttl = defaultRepairCodeTTL
	}
	code = randHumanCode()
	expiresAt = time.Now().Add(ttl)
	s.repairCode = repairCodeState{Value: code, ExpiresAt: expiresAt}
	return code, expiresAt, nil
}

func (s *Store) ResetClaim(repairCode string) (claimCode string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Claimed {
		return "", ErrRepairNotAllowed
	}
	now := time.Now()
	if s.repairCode.Value == "" {
		return "", ErrInvalidRepairCode
	}
	if now.After(s.repairCode.ExpiresAt) {
		s.repairCode = repairCodeState{}
		return "", ErrRepairCodeExpired
	}
	if subtle.ConstantTimeCompare([]byte(normalizeHumanCode(repairCode)), []byte(normalizeHumanCode(s.repairCode.Value))) != 1 {
		return "", ErrInvalidRepairCode
	}

	s.state.Claimed = false
	s.state.BearerToken = ""
	s.state.KeyID = ""
	s.state.AccessTokenExpiresAt = time.Time{}
	s.state.RefreshToken = ""
	s.state.RefreshTokenExpiresAt = time.Time{}
	s.state.ClaimCode = randHumanCode()
	s.challenges = map[string]Challenge{}
	s.repairCode = repairCodeState{}

	if err := s.persistLocked(); err != nil {
		return "", err
	}
	return s.state.ClaimCode, nil
}

func (s *Store) load(initialClaimCode string) error {
	initialClaimCode = strings.TrimSpace(initialClaimCode)
	_, statErr := os.Stat(s.path)
	missing := os.IsNotExist(statErr)
	if statErr != nil && !missing {
		return fmt.Errorf("stat config path: %w", statErr)
	}

	cfg, err := config.Load(s.path)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	loaded := persistedState{
		DeviceID:              cfg.Auth.DeviceID,
		Claimed:               cfg.Auth.Claimed,
		ClaimCode:             cfg.Auth.ClaimCode,
		BearerToken:           cfg.Auth.BearerToken,
		KeyID:                 cfg.Auth.KeyID,
		AccessTokenExpiresAt:  cfg.Auth.AccessTokenExpiresAt,
		RefreshToken:          cfg.Auth.RefreshToken,
		RefreshTokenExpiresAt: cfg.Auth.RefreshTokenExpiresAt,
	}

	loaded.DeviceID = s.preferredDeviceID
	loaded.ClaimCode = strings.TrimSpace(loaded.ClaimCode)
	loaded.BearerToken = strings.TrimSpace(loaded.BearerToken)
	loaded.KeyID = strings.TrimSpace(loaded.KeyID)
	loaded.RefreshToken = strings.TrimSpace(loaded.RefreshToken)

	if loaded.ClaimCode == "" {
		if missing {
			if initialClaimCode == "" {
				return errors.New("initial claim code is required")
			}
			loaded.ClaimCode = initialClaimCode
		}
		if initialClaimCode == "" {
			return errors.New("initial claim code is required")
		}
		loaded.ClaimCode = initialClaimCode
	}

	if loaded.Claimed {
		if loaded.BearerToken == "" {
			loaded.Claimed = false
		}
		if loaded.AccessTokenExpiresAt.IsZero() {
			loaded.AccessTokenExpiresAt = time.Now().Add(defaultAccessTokenTTL)
		}
		if loaded.RefreshToken == "" {
			loaded.RefreshToken = randHex(48)
		}
		if loaded.RefreshTokenExpiresAt.IsZero() {
			loaded.RefreshTokenExpiresAt = time.Now().Add(defaultRefreshTokenTTL)
		}
	}
	if !loaded.Claimed {
		loaded.BearerToken = ""
		loaded.KeyID = ""
		loaded.AccessTokenExpiresAt = time.Time{}
		loaded.RefreshToken = ""
		loaded.RefreshTokenExpiresAt = time.Time{}
	}
	s.state = loaded
	return s.persistLocked()
}

func (s *Store) persistLocked() error {
	cfg, err := config.Load(s.path)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.Auth = config.AuthState{
		DeviceID:              s.state.DeviceID,
		Claimed:               s.state.Claimed,
		ClaimCode:             s.state.ClaimCode,
		BearerToken:           s.state.BearerToken,
		KeyID:                 s.state.KeyID,
		AccessTokenExpiresAt:  s.state.AccessTokenExpiresAt,
		RefreshToken:          s.state.RefreshToken,
		RefreshTokenExpiresAt: s.state.RefreshTokenExpiresAt,
	}
	return config.Save(s.path, cfg)
}

func randHex(nBytes int) string {
	buf := make([]byte, nBytes)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func randHumanCode() string {
	return fmt.Sprintf("%s-%s", randHex(2), randHex(2))
}

func deriveDeviceID(model string, mac string) string {
	modelPrefix := normalizeModelPrefix(model)
	macHex := normalizeMACHex(mac)
	if modelPrefix == "" || macHex == "" {
		return ""
	}
	return modelPrefix + "_" + macHex
}

func normalizeModelPrefix(model string) string {
	upper := strings.ToUpper(strings.TrimSpace(model))
	switch {
	case strings.Contains(upper, "C300X"):
		return "c300x"
	case strings.Contains(upper, "C100X"):
		return "c100x"
	default:
		return ""
	}
}

func normalizeMACHex(mac string) string {
	raw := strings.ToLower(strings.TrimSpace(mac))
	if raw == "" {
		return ""
	}
	buf := make([]byte, 0, 12)
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			buf = append(buf, ch)
		}
	}
	if len(buf) != 12 {
		return ""
	}
	return string(buf)
}

func (s *Store) hasClaimedBearerLocked() bool {
	return s.state.Claimed && s.state.BearerToken != ""
}

func (s *Store) rotateBearerLocked() {
	s.state.BearerToken = randHex(32)
	s.state.KeyID = "kid_" + randHex(8)
	s.state.AccessTokenExpiresAt = time.Now().Add(defaultAccessTokenTTL)
}

func (s *Store) rotateRefreshLocked() {
	s.state.RefreshToken = randHex(48)
	s.state.RefreshTokenExpiresAt = time.Now().Add(defaultRefreshTokenTTL)
}

func (s *Store) pruneExpiredChallengesLocked() {
	now := time.Now()
	for id, challenge := range s.challenges {
		if now.After(challenge.ExpiresAt) {
			delete(s.challenges, id)
		}
	}
}

func normalizeHumanCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

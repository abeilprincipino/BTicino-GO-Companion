package auth

import (
	"bticino-go-companion/internal/config"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	bearerTokenBytes = 32
	challengeBytes   = 16

	challengeLifetime  = 5 * time.Minute
	RepairCodeLifetime = 10 * time.Minute
	rateWindow         = time.Minute
	maxAttempts        = 5
)

var (
	ErrStoreUnavailable        = errors.New("auth store is unavailable")
	ErrInvalidSourceIP         = errors.New("invalid source ip")
	ErrRateLimited             = errors.New("claim attempts rate limited")
	ErrChallengeNotFound       = errors.New("challenge not found")
	ErrChallengeExpired        = errors.New("challenge expired")
	ErrChallengeSourceMismatch = errors.New("challenge source mismatch")
	ErrInvalidClaimCode        = errors.New("invalid claim code")
	ErrAlreadyClaimed          = errors.New("device already claimed")
	ErrRepairNotAllowed        = errors.New("repair flow is not allowed")
	ErrInvalidRepairCode       = errors.New("invalid repair code")
	ErrRepairCodeExpired       = errors.New("repair code expired")
)

type Challenge struct {
	ID        string
	ExpiresAt time.Time
}

type challenge struct {
	sourceIP  string
	expiresAt time.Time
}

type attempts struct {
	windowStart time.Time
	count       int
}

type repairCode struct {
	value     string
	expiresAt time.Time
}

type Store struct {
	config *config.Store
	now    func() time.Time
	logger *slog.Logger

	mu         sync.Mutex
	challenges map[string]challenge
	attempts   map[string]attempts
	repair     repairCode
}

func NewStore(cfg *config.Store) *Store {
	return &Store{
		config:     cfg,
		now:        time.Now,
		logger:     slog.Default(),
		challenges: make(map[string]challenge),
		attempts:   make(map[string]attempts),
	}
}

func (s *Store) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Store) CreateChallenge(sourceIP string) (Challenge, error) {
	sourceIP, err := canonicalIP(sourceIP)
	if err != nil {
		return Challenge{}, err
	}

	if s.config == nil {
		return Challenge{}, ErrStoreUnavailable
	}
	if !s.NeedsClaim() {
		return Challenge{}, ErrAlreadyClaimed
	}

	id, err := config.RandomHex(challengeBytes)
	if err != nil {
		return Challenge{}, fmt.Errorf("generate challenge: %w", err)
	}

	now := s.now()
	expiresAt := now.Add(challengeLifetime)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.removeExpiredChallenges(now)
	s.removeExpiredAttempts(now)
	s.challenges[id] = challenge{sourceIP: sourceIP, expiresAt: expiresAt}

	return Challenge{ID: id, ExpiresAt: expiresAt}, nil
}

func (s *Store) Claim(sourceIP, challengeID, claimCode string) (string, error) {
	sourceIP, err := canonicalIP(sourceIP)
	if err != nil {
		return "", err
	}

	if s.config == nil {
		return "", ErrStoreUnavailable
	}
	if !s.NeedsClaim() {
		return "", ErrAlreadyClaimed
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.removeExpiredAttempts(now)

	if !s.allowAttempt(sourceIP, now) {
		return "", ErrRateLimited
	}

	challenge, ok := s.challenges[challengeID]
	if !ok {
		return "", ErrChallengeNotFound
	}

	if !challenge.expiresAt.After(now) {
		delete(s.challenges, challengeID)
		return "", ErrChallengeExpired
	}

	if challenge.sourceIP != sourceIP {
		return "", ErrChallengeSourceMismatch
	}

	token, err := config.RandomHex(bearerTokenBytes)
	if err != nil {
		return "", fmt.Errorf("generate bearer token: %w", err)
	}

	if err := s.config.Update(func(cfg *config.Config) error {
		if !constantTimeClaimCodeEqual(claimCode, cfg.Auth.ClaimCode) {
			return ErrInvalidClaimCode
		}

		cfg.Auth.BearerTokenHash = bearerTokenHash(token)
		cfg.Auth.ClaimCode = ""

		return nil
	}); err != nil {
		return "", err
	}

	delete(s.challenges, challengeID)

	return token, nil
}

func (s *Store) ValidateBearer(token string) bool {
	if s.config == nil {
		return false
	}

	return constantTimeHexEqual(bearerTokenHash(token), s.config.Snapshot().Auth.BearerTokenHash, sha256.Size)
}

func (s *Store) NeedsClaim() bool {
	if s.config == nil {
		return true
	}

	return s.config.Snapshot().Auth.BearerTokenHash == ""
}

func (s *Store) IssueInitialClaimCode() (string, error) {
	if s.config == nil {
		return "", ErrStoreUnavailable
	}
	if !s.NeedsClaim() {
		return "", ErrAlreadyClaimed
	}

	code, err := config.GenerateClaimCode()
	if err != nil {
		return "", fmt.Errorf("generate claim code: %w", err)
	}
	if err := s.config.Update(func(cfg *config.Config) error {
		if cfg.Auth.ClaimCode != "" {
			return ErrAlreadyClaimed
		}
		cfg.Auth.ClaimCode = code
		return nil
	}); err != nil {
		return "", err
	}
	return code, nil
}

func (s *Store) RotateBearer() (string, error) {
	if s.config == nil {
		return "", ErrStoreUnavailable
	}

	token, err := config.RandomHex(bearerTokenBytes)
	if err != nil {
		return "", fmt.Errorf("generate bearer token: %w", err)
	}

	if err := s.config.Update(func(cfg *config.Config) error {
		cfg.Auth.BearerTokenHash = bearerTokenHash(token)
		cfg.Auth.ClaimCode = ""
		return nil
	}); err != nil {
		return "", err
	}

	return token, nil
}

func (s *Store) RevokeBearer() error {
	if s.config == nil {
		return ErrStoreUnavailable
	}

	return s.config.Update(func(cfg *config.Config) error {
		cfg.Auth.BearerTokenHash = ""
		return nil
	})
}

func (s *Store) IssueRepairCode() (string, time.Time, error) {
	if s.NeedsClaim() {
		return "", time.Time{}, ErrRepairNotAllowed
	}

	code, err := config.GenerateClaimCode()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate repair code: %w", err)
	}

	expiresAt := s.now().Add(RepairCodeLifetime)
	s.mu.Lock()
	s.repair = repairCode{value: code, expiresAt: expiresAt}
	s.mu.Unlock()
	s.logger.Info("repair code issued", "expires_at", expiresAt)

	return code, expiresAt, nil
}

func (s *Store) RecoverBearer(code string) (string, error) {
	if s.config == nil {
		return "", ErrStoreUnavailable
	}
	if s.NeedsClaim() {
		return "", ErrRepairNotAllowed
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repair.value == "" || !constantTimeStringEqual(code, s.repair.value) {
		s.logger.Warn("repair code rejected")
		return "", ErrInvalidRepairCode
	}
	if !s.repair.expiresAt.After(s.now()) {
		s.repair = repairCode{}
		s.logger.Info("repair code expired")
		return "", ErrRepairCodeExpired
	}

	token, err := config.RandomHex(bearerTokenBytes)
	if err != nil {
		return "", fmt.Errorf("generate bearer token: %w", err)
	}
	if err := s.config.Update(func(cfg *config.Config) error {
		cfg.Auth.BearerTokenHash = bearerTokenHash(token)
		return nil
	}); err != nil {
		return "", err
	}

	s.repair = repairCode{}
	s.logger.Info("repair code consumed")
	s.logger.Info("bearer recovery completed")
	return token, nil
}

func (s *Store) allowAttempt(sourceIP string, now time.Time) bool {
	attempt := s.attempts[sourceIP]
	if now.Sub(attempt.windowStart) >= rateWindow {
		attempt = attempts{windowStart: now}
	}

	if attempt.count >= maxAttempts {
		s.attempts[sourceIP] = attempt
		return false
	}

	attempt.count++
	s.attempts[sourceIP] = attempt

	return true
}

func (s *Store) removeExpiredChallenges(now time.Time) {
	for id, challenge := range s.challenges {
		if !challenge.expiresAt.After(now) {
			delete(s.challenges, id)
			s.logger.Info("pairing challenge expired")
		}
	}
}

func (s *Store) removeExpiredAttempts(now time.Time) {
	for sourceIP, attempt := range s.attempts {
		if now.Sub(attempt.windowStart) >= rateWindow {
			delete(s.attempts, sourceIP)
		}
	}
}

func canonicalIP(sourceIP string) (string, error) {
	ip, err := netip.ParseAddr(sourceIP)
	if err != nil {
		return "", ErrInvalidSourceIP
	}

	return ip.Unmap().String(), nil
}

func constantTimeHexEqual(left, right string, size int) bool {
	leftBytes, leftValid := decodeHex(left, size)
	rightBytes, rightValid := decodeHex(right, size)

	return subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1 && leftValid && rightValid
}

func bearerTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func constantTimeClaimCodeEqual(left, right string) bool {
	if !config.ValidClaimCode(left) || !config.ValidClaimCode(right) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func constantTimeStringEqual(left, right string) bool {
	left = strings.ToLower(strings.TrimSpace(left))
	right = strings.ToLower(strings.TrimSpace(right))
	if len(left) != len(right) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func decodeHex(value string, size int) ([]byte, bool) {
	decoded := make([]byte, size)
	if len(value) != size*2 {
		return decoded, false
	}

	_, err := hex.Decode(decoded, []byte(value))

	return decoded, err == nil
}

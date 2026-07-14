package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"bticino-go-companion/internal/config"
)

const (
	claimCodeBytes   = 4
	bearerTokenBytes = 32
	challengeBytes   = 16

	challengeLifetime = 5 * time.Minute
	rateWindow        = time.Minute
	maxAttempts       = 5
)

var (
	ErrStoreUnavailable        = errors.New("auth store is unavailable")
	ErrInvalidSourceIP         = errors.New("invalid source ip")
	ErrRateLimited             = errors.New("claim attempts rate limited")
	ErrChallengeNotFound       = errors.New("challenge not found")
	ErrChallengeExpired        = errors.New("challenge expired")
	ErrChallengeSourceMismatch = errors.New("challenge source mismatch")
	ErrInvalidClaimCode        = errors.New("invalid claim code")
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

type Store struct {
	config *config.Store
	now    func() time.Time

	mu         sync.Mutex
	challenges map[string]challenge
	attempts   map[string]attempts
}

func NewStore(cfg *config.Store) *Store {
	return &Store{
		config:     cfg,
		now:        time.Now,
		challenges: make(map[string]challenge),
		attempts:   make(map[string]attempts),
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

	id, err := randomHex(challengeBytes)
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

func (s *Store) Claim(sourceIP, challengeID, repairCode string) (string, error) {
	sourceIP, err := canonicalIP(sourceIP)
	if err != nil {
		return "", err
	}
	if s.config == nil {
		return "", ErrStoreUnavailable
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

	token, err := randomHex(bearerTokenBytes)
	if err != nil {
		return "", fmt.Errorf("generate bearer token: %w", err)
	}
	nextRepairCode, err := randomHex(claimCodeBytes)
	if err != nil {
		return "", fmt.Errorf("generate repair code: %w", err)
	}
	if err := s.config.Update(func(cfg *config.Config) error {
		if !constantTimeHexEqual(repairCode, cfg.Auth.ClaimCode, claimCodeBytes) {
			return ErrInvalidClaimCode
		}
		cfg.Auth.BearerToken = token
		cfg.Auth.ClaimCode = nextRepairCode
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
	return constantTimeHexEqual(token, s.config.Snapshot().Auth.BearerToken, bearerTokenBytes)
}

func (s *Store) RotateBearer() (string, error) {
	if s.config == nil {
		return "", ErrStoreUnavailable
	}
	token, err := randomHex(bearerTokenBytes)
	if err != nil {
		return "", fmt.Errorf("generate bearer token: %w", err)
	}
	if err := s.config.Update(func(cfg *config.Config) error {
		cfg.Auth.BearerToken = token
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
		cfg.Auth.BearerToken = ""
		return nil
	})
}

func (s *Store) IssueRepairCode() (string, error) {
	return s.replaceRepairCode(false)
}

func (s *Store) ResetRepairCode() (string, error) {
	return s.replaceRepairCode(true)
}

func (s *Store) replaceRepairCode(revokeBearer bool) (string, error) {
	if s.config == nil {
		return "", ErrStoreUnavailable
	}
	repairCode, err := randomHex(claimCodeBytes)
	if err != nil {
		return "", fmt.Errorf("generate repair code: %w", err)
	}
	if err := s.config.Update(func(cfg *config.Config) error {
		cfg.Auth.ClaimCode = repairCode
		if revokeBearer {
			cfg.Auth.BearerToken = ""
		}
		return nil
	}); err != nil {
		return "", err
	}
	return repairCode, nil
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

func decodeHex(value string, size int) ([]byte, bool) {
	decoded := make([]byte, size)
	if len(value) != size*2 {
		return decoded, false
	}
	_, err := hex.Decode(decoded, []byte(value))
	return decoded, err == nil
}

func randomHex(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

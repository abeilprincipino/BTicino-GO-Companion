package auth

import (
	"bticino-go-companion/internal/config"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_ClaimLifecycle(t *testing.T) {
	t.Parallel()

	store, backend := newTestStore(t)

	code, err := store.InitialClaimCode()
	if err != nil {
		t.Fatalf("initial claim code: %v", err)
	}

	challenge, err := store.CreateChallenge("192.0.2.1")
	if err != nil {
		t.Fatalf("create challenge: %v", err)
	}

	token, err := store.Claim("192.0.2.1", challenge.ID, code)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if len(token) != bearerTokenBytes*2 {
		t.Fatalf("token length = %d, want %d", len(token), bearerTokenBytes*2)
	}

	if !store.ValidateBearer(token) {
		t.Fatal("issued bearer token was not valid")
	}

	if stored := backend.Snapshot().Auth.BearerTokenHash; stored == token || len(stored) != 64 {
		t.Fatalf("persisted bearer value = %q, want SHA-256 hash", stored)
	}

	if _, err := store.Claim("192.0.2.1", challenge.ID, code); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("replayed claim error = %v, want ErrAlreadyClaimed", err)
	}

	if backend.Snapshot().Auth.PairingState != config.PairingStateClaimed {
		t.Fatal("successful claim did not update pairing state")
	}

	if _, err := store.CreateChallenge("192.0.2.1"); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("challenge after claim error = %v, want ErrAlreadyClaimed", err)
	}
}

func TestStore_ClaimRejectsInvalidChallenges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		source  string
		advance time.Duration
		wantErr error
	}{
		{name: "different source", source: "192.0.2.2", wantErr: ErrChallengeSourceMismatch},
		{name: "expired", source: "192.0.2.1", advance: challengeLifetime, wantErr: ErrChallengeExpired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, _ := newTestStore(t)

			challenge, err := store.CreateChallenge("192.0.2.1")
			if err != nil {
				t.Fatalf("create challenge: %v", err)
			}

			store.now = func() time.Time { return testNow.Add(tt.advance) }

			code, err := store.InitialClaimCode()
			if err != nil {
				t.Fatalf("initial claim code: %v", err)
			}

			_, err = store.Claim(tt.source, challenge.ID, code)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("claim error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestStore_ClaimRateLimit(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)

	code, err := store.InitialClaimCode()
	if err != nil {
		t.Fatalf("initial claim code: %v", err)
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		challenge, err := store.CreateChallenge("192.0.2.1")
		if err != nil {
			t.Fatalf("create challenge %d: %v", attempt, err)
		}

		if _, err := store.Claim("192.0.2.1", challenge.ID, "00000000"); !errors.Is(err, ErrInvalidClaimCode) {
			t.Fatalf("attempt %d error = %v, want ErrInvalidClaimCode", attempt, err)
		}
	}

	challenge, err := store.CreateChallenge("192.0.2.1")
	if err != nil {
		t.Fatalf("create limited challenge: %v", err)
	}

	if _, err := store.Claim("192.0.2.1", challenge.ID, code); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("limited claim error = %v, want ErrRateLimited", err)
	}

	store.now = func() time.Time { return testNow.Add(rateWindow) }

	challenge, err = store.CreateChallenge("192.0.2.1")
	if err != nil {
		t.Fatalf("create reset challenge: %v", err)
	}

	if _, err := store.Claim("192.0.2.1", challenge.ID, code); err != nil {
		t.Fatalf("claim after rate window: %v", err)
	}
}

func TestStore_ValidateBearer(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)

	token, err := store.RotateBearer()
	if err != nil {
		t.Fatalf("rotate bearer: %v", err)
	}

	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "current token", token: token, want: true},
		{name: "wrong token", token: "0000000000000000000000000000000000000000000000000000000000000000", want: false},
		{name: "wrong length", token: "abcd", want: false},
		{name: "non hexadecimal", token: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := store.ValidateBearer(tt.token); got != tt.want {
				t.Fatalf("ValidateBearer(%q) = %t, want %t", tt.token, got, tt.want)
			}
		})
	}
}

func TestStore_RotateAndRevokeBearer(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)

	first, err := store.RotateBearer()
	if err != nil {
		t.Fatalf("rotate first bearer: %v", err)
	}

	second, err := store.RotateBearer()
	if err != nil {
		t.Fatalf("rotate second bearer: %v", err)
	}

	if store.ValidateBearer(first) {
		t.Fatal("rotated bearer remains valid")
	}

	if !store.ValidateBearer(second) {
		t.Fatal("new bearer is not valid")
	}

	if err := store.RevokeBearer(); err != nil {
		t.Fatalf("revoke bearer: %v", err)
	}

	if store.ValidateBearer(second) {
		t.Fatal("revoked bearer remains valid")
	}
}

func TestStore_RepairCodeIssueAndRecoverBearer(t *testing.T) {
	t.Parallel()

	store, backend := newTestStore(t)

	token, err := store.RotateBearer()
	if err != nil {
		t.Fatalf("rotate bearer: %v", err)
	}

	issued, expiresAt, err := store.IssueRepairCode()
	if err != nil {
		t.Fatalf("issue repair code: %v", err)
	}

	if !config.ValidClaimCode(issued) {
		t.Fatalf("issued repair code = %q", issued)
	}

	if !expiresAt.After(testNow) {
		t.Fatalf("repair code expires at %s", expiresAt)
	}

	if !store.ValidateBearer(token) {
		t.Fatal("issuing repair code revoked bearer")
	}

	if _, err := store.RecoverBearer("invalid"); !errors.Is(err, ErrInvalidRepairCode) {
		t.Fatalf("invalid repair code error = %v, want ErrInvalidRepairCode", err)
	}

	replacement, err := store.RecoverBearer(issued)
	if err != nil {
		t.Fatalf("recover bearer: %v", err)
	}

	if !store.ValidateBearer(replacement) {
		t.Fatal("recovered bearer is not valid")
	}

	if store.ValidateBearer(token) {
		t.Fatal("reset repair code did not revoke bearer")
	}

	if backend.Snapshot().Auth.PairingState != config.PairingStateClaimed {
		t.Fatalf("pairing state = %q, want claimed", backend.Snapshot().Auth.PairingState)
	}
}

func TestStore_RejectsInvalidSourceIP(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	if _, err := store.CreateChallenge("not-an-ip"); !errors.Is(err, ErrInvalidSourceIP) {
		t.Fatalf("create challenge error = %v, want ErrInvalidSourceIP", err)
	}
}

func newTestStore(t *testing.T) (*Store, *config.Store) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.Create(path, config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}); err != nil {
		t.Fatalf("create config: %v", err)
	}

	backend, err := config.Open(path)
	if err != nil {
		t.Fatalf("open config store: %v", err)
	}

	store := NewStore(backend)
	store.now = func() time.Time { return testNow }

	if err := backend.Update(func(cfg *config.Config) error {
		cfg.WebUI.SessionSecret = "test-session-secret"
		return nil
	}); err != nil {
		t.Fatalf("set session secret: %v", err)
	}

	if _, err := store.StartInitialClaim(); err != nil {
		t.Fatalf("issue initial claim code: %v", err)
	}

	return store, backend
}

var testNow = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

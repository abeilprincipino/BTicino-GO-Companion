package auth

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(
		filepath.Join(t.TempDir(), "config.json"),
		"abcd-1234",
		"C300X",
		"00:03:50:96:2e:38",
	)
	if err != nil {
		t.Fatalf("new test store: %v", err)
	}
	return store
}

func TestStoreUsesMACBasedDeviceID(t *testing.T) {
	store := newTestStore(t)
	if got, want := store.DeviceID(), "c300x_000350962e38"; got != want {
		t.Fatalf("unexpected device id: got %q want %q", got, want)
	}
}

func TestIssueRepairCodeAndResetClaim(t *testing.T) {
	store := newTestStore(t)
	ch, err := store.StartChallenge()
	if err != nil {
		t.Fatalf("start challenge: %v", err)
	}
	if _, _, err := store.Claim(ClaimRequest{
		ChallengeID: ch.ID,
		Nonce:       ch.Nonce,
		ClaimCode:   "abcd-1234",
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	code, _, err := store.IssueRepairCode(2 * time.Minute)
	if err != nil {
		t.Fatalf("issue repair code: %v", err)
	}
	if code == "" {
		t.Fatalf("expected non-empty repair code")
	}

	if _, err := store.ResetClaim("wrong-code"); !errors.Is(err, ErrInvalidRepairCode) {
		t.Fatalf("expected ErrInvalidRepairCode, got %v", err)
	}

	newClaimCode, err := store.ResetClaim(code)
	if err != nil {
		t.Fatalf("reset claim: %v", err)
	}
	if newClaimCode == "" {
		t.Fatalf("expected non-empty new claim code")
	}
	if !store.NeedsClaim() {
		t.Fatalf("expected needs claim after reset")
	}
}

func TestRevokeAndReplaceTrimsKeyID(t *testing.T) {
	store := newTestStore(t)
	ch, err := store.StartChallenge()
	if err != nil {
		t.Fatalf("start challenge: %v", err)
	}
	_, keyID, err := store.Claim(ClaimRequest{
		ChallengeID: ch.ID,
		Nonce:       ch.Nonce,
		ClaimCode:   "abcd-1234",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, _, err := store.RevokeAndReplace("  " + keyID + "  "); err != nil {
		t.Fatalf("revoke and replace with spaced key id: %v", err)
	}
}

func TestResetClaimRepairCodeIsCaseInsensitive(t *testing.T) {
	store := newTestStore(t)
	ch, err := store.StartChallenge()
	if err != nil {
		t.Fatalf("start challenge: %v", err)
	}
	if _, _, err := store.Claim(ClaimRequest{
		ChallengeID: ch.ID,
		Nonce:       ch.Nonce,
		ClaimCode:   "abcd-1234",
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	code, _, err := store.IssueRepairCode(time.Minute)
	if err != nil {
		t.Fatalf("issue repair code: %v", err)
	}

	if _, err := store.ResetClaim(strings.ToUpper(code)); err != nil {
		t.Fatalf("reset claim with upper-case repair code: %v", err)
	}
}

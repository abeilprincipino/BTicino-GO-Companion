package security

import (
	"testing"
	"time"
)

func TestGuardLocksIPAfterRepeatedFailures(t *testing.T) {
	guard := NewGuard()
	ip := "192.0.2.10"

	for range guard.pairing.Policy.FailureThreshold {
		if decision := guard.Begin(ScopePairing, ip); !decision.Allowed {
			t.Fatalf("begin should be allowed before threshold, got %+v", decision)
		}
		guard.Failure(ScopePairing, ip)
	}

	decision := guard.Begin(ScopePairing, ip)
	if decision.Allowed {
		t.Fatal("expected ip lockout after repeated failures")
	}
	if decision.Code != "locked_ip" {
		t.Fatalf("expected locked_ip, got %s", decision.Code)
	}
	if decision.RetryAfter <= 0 || decision.RetryAfter > guard.pairing.Policy.MaxLockout {
		t.Fatalf("unexpected retry after: %s", decision.RetryAfter)
	}
}

func TestGuardSuccessClearsLockout(t *testing.T) {
	guard := NewGuard()
	ip := "192.0.2.20"

	guard.Failure(ScopeAuth, ip)
	guard.auth.PerIP[ip].LockedUntil = time.Now().Add(time.Minute)
	guard.auth.GlobalFailures = 3
	guard.auth.GlobalLockedTil = time.Now().Add(time.Minute)

	guard.Success(ScopeAuth, ip)

	if decision := guard.Begin(ScopeAuth, ip); !decision.Allowed {
		t.Fatalf("expected success to clear lockout, got %+v", decision)
	}
}

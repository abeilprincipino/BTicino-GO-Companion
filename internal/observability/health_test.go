package observability

import (
	"testing"
	"time"

	"bticino-go-companion/internal/services/runtime"
)

func TestHealthReady(t *testing.T) {
	status := runtime.Snapshot{
		SIP:        runtime.ComponentStatus{Enabled: true, Ready: true},
		OpenWebNet: runtime.ComponentStatus{Enabled: true, Ready: true},
		Control:    runtime.ComponentStatus{Enabled: true, Ready: true},
	}
	health := New(time.Now().UTC(), status)
	if !health.Live || !health.Ready || health.Degraded {
		t.Fatalf("unexpected health: %+v", health)
	}
}

func TestHealthNotReadyAndDegraded(t *testing.T) {
	status := runtime.Snapshot{
		SIP:        runtime.ComponentStatus{Enabled: true, Ready: false, Error: "bind failed"},
		OpenWebNet: runtime.ComponentStatus{Enabled: true, Ready: true},
		Control:    runtime.ComponentStatus{Enabled: true, Ready: false},
	}
	health := New(time.Now().UTC(), status)
	if health.Ready {
		t.Fatalf("health should not be ready: %+v", health)
	}
	if !health.Degraded {
		t.Fatalf("health should be degraded: %+v", health)
	}
	if len(health.Reasons) == 0 {
		t.Fatalf("expected readiness reasons: %+v", health)
	}
}

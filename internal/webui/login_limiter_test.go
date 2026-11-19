package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"bticino-go-companion/internal/auth"
)

func loginFrom(t *testing.T, server *Server, address, password string) *httptest.ResponseRecorder {
	t.Helper()

	data, err := json.Marshal(loginRequest{Username: auth.DefaultOwnerUsername, Password: password})
	if err != nil {
		t.Fatalf("marshal login: %v", err)
	}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webui/api/login", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = address + ":40000"

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	return response
}

func TestLoginLocksOutAnAddressAfterRepeatedFailures(t *testing.T) {
	t.Parallel()

	server, _ := testServer(t, nil)
	now := time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)
	server.logins.now = func() time.Time { return now }

	for attempt := 1; attempt <= maxLoginFailures; attempt++ {
		if response := loginFrom(t, server, "192.168.1.50", "wrong"); response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", attempt, response.Code)
		}
	}

	// Locked out: even the right password waits, and says for how long.
	locked := loginFrom(t, server, "192.168.1.50", auth.DefaultOwnerPassword)
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("locked status = %d, want 429", locked.Code)
	}

	if got := locked.Header().Get("Retry-After"); got != strconv.Itoa(int(firstLoginLockout/time.Second)) {
		t.Fatalf("Retry-After = %q, want %d", got, int(firstLoginLockout/time.Second))
	}

	// Another address is not affected.
	if response := loginFrom(t, server, "192.168.1.51", auth.DefaultOwnerPassword); response.Code != http.StatusOK {
		t.Fatalf("other address status = %d, want 200", response.Code)
	}

	// The lockout ends by itself, and a success forgets the failures.
	now = now.Add(firstLoginLockout)

	if response := loginFrom(t, server, "192.168.1.50", auth.DefaultOwnerPassword); response.Code != http.StatusOK {
		t.Fatalf("after lockout status = %d, want 200", response.Code)
	}

	if response := loginFrom(t, server, "192.168.1.50", "wrong"); response.Code != http.StatusUnauthorized {
		t.Fatalf("after success status = %d, want 401", response.Code)
	}
}

func TestLoginLockoutDoublesUpToTheCap(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter()
	limiter.now = func() time.Time { return now }

	var lockouts []time.Duration

	for round := 0; round < 6; round++ {
		var lockout time.Duration
		for range maxLoginFailures {
			lockout = limiter.fail("203.0.113.7")
		}

		lockouts = append(lockouts, lockout)
		now = now.Add(lockout)
	}

	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, maxLoginLockout, maxLoginLockout}
	for i := range want {
		if lockouts[i] != want[i] {
			t.Fatalf("lockouts = %v, want %v", lockouts, want)
		}
	}

	// An address quiet for long enough starts again from the first lockout.
	now = now.Add(loginFailureMemory)

	var lockout time.Duration
	for range maxLoginFailures {
		lockout = limiter.fail("203.0.113.7")
	}

	if lockout != firstLoginLockout {
		t.Fatalf("lockout after a quiet hour = %v, want %v", lockout, firstLoginLockout)
	}
}

func TestLoginLimiterBoundsItsTable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter()
	limiter.now = func() time.Time { return now }

	for i := range maxTrackedLoginAddresses + 50 {
		now = now.Add(time.Millisecond)
		limiter.fail("198.51.100." + strconv.Itoa(i))
	}

	if got := len(limiter.addresses); got > maxTrackedLoginAddresses {
		t.Fatalf("tracked addresses = %d, want at most %d", got, maxTrackedLoginAddresses)
	}
}

func TestSameOriginAcceptsHTTPSBehindAReverseProxy(t *testing.T) {
	t.Parallel()

	server, _ := testServer(t, nil)

	tests := []struct {
		origin string
		want   int
	}{
		{origin: "http://intercom.example.com", want: http.StatusOK},
		{origin: "https://intercom.example.com", want: http.StatusOK},
		{origin: "https://other.example.com", want: http.StatusForbidden},
		{origin: "https://intercom.example.com.evil.test", want: http.StatusForbidden},
		{origin: "ftp://intercom.example.com", want: http.StatusForbidden},
		{origin: "null", want: http.StatusForbidden},
	}

	for _, test := range tests {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webui/api/admin/logout", nil)
		request.Host = "intercom.example.com"
		request.Header.Set("Origin", test.origin)

		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)

		if response.Code != test.want {
			t.Fatalf("Origin %q: status = %d, want %d", test.origin, response.Code, test.want)
		}
	}
}

package webui

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// maxLoginFailures failed logins from one address lock it out.
	maxLoginFailures = 5
	// firstLoginLockout is the first lockout; each later one doubles, up to
	// maxLoginLockout. The lockout is temporary on every address, private ones
	// included: a forgotten password on the LAN costs a wait, never the device.
	firstLoginLockout = time.Minute
	maxLoginLockout   = 15 * time.Minute
	// loginFailureMemory is how long an address with no new failure is
	// remembered, lockout level included.
	loginFailureMemory = time.Hour
	// maxTrackedLoginAddresses bounds the table, so a flood of addresses cannot
	// grow it without limit.
	maxTrackedLoginAddresses = 1024
)

type loginFailures struct {
	count       int
	lockouts    int
	lockedUntil time.Time
	lastFailure time.Time
}

// loginLimiter slows password guessing on the WebUI login. It keys on the
// connecting address: behind a reverse proxy that is the proxy, so every
// client of the proxy shares one budget, while direct LAN access keeps its own.
// Forwarded headers are not trusted, since any client could set them.
type loginLimiter struct {
	mu        sync.Mutex
	addresses map[string]loginFailures
	now       func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{addresses: make(map[string]loginFailures), now: time.Now}
}

// retryAfter reports how long address must still wait, or zero when it may
// try to log in.
func (l *loginLimiter) retryAfter(address string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	wait := l.addresses[address].lockedUntil.Sub(l.now())
	if wait < 0 {
		return 0
	}

	return wait
}

// fail records a failed login and returns the lockout it starts, or zero.
func (l *loginLimiter) fail(address string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.forgetIdle(now)

	failures := l.addresses[address]
	failures.count++
	failures.lastFailure = now

	var lockout time.Duration

	if failures.count >= maxLoginFailures {
		lockout = firstLoginLockout << failures.lockouts
		if lockout <= 0 || lockout > maxLoginLockout {
			lockout = maxLoginLockout
		}

		failures.count = 0
		failures.lockouts++
		failures.lockedUntil = now.Add(lockout)
	}

	l.addresses[address] = failures

	return lockout
}

// succeed forgets the failures of address.
func (l *loginLimiter) succeed(address string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.addresses, address)
}

// forgetIdle drops the addresses with no recent failure and no running
// lockout, then the oldest ones while the table is still full.
func (l *loginLimiter) forgetIdle(now time.Time) {
	for address, failures := range l.addresses {
		if now.Sub(failures.lastFailure) >= loginFailureMemory && !now.Before(failures.lockedUntil) {
			delete(l.addresses, address)
		}
	}

	for len(l.addresses) >= maxTrackedLoginAddresses {
		var (
			oldest     string
			oldestSeen time.Time
		)

		for address, failures := range l.addresses {
			if oldest == "" || failures.lastFailure.Before(oldestSeen) {
				oldest, oldestSeen = address, failures.lastFailure
			}
		}

		delete(l.addresses, oldest)
	}
}

// loginAddress is the connecting address of r, without its port.
func loginAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// writeLoginLockout answers a locked-out login with 429 and Retry-After.
func writeLoginLockout(w http.ResponseWriter, wait time.Duration) {
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}

	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, "too many failed logins; try again in "+strconv.Itoa(seconds)+" s")
}

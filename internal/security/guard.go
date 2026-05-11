package security

import (
	"strings"
	"sync"
	"time"
)

type Scope string

const (
	ScopePairing Scope = "pairing"
	ScopeAuth    Scope = "auth"
)

type Decision struct {
	Allowed    bool
	Code       string
	Message    string
	RetryAfter time.Duration
}

type policy struct {
	Window           time.Duration
	PerIPLimit       int
	GlobalLimit      int
	FailureThreshold int
	BaseLockout      time.Duration
	MaxLockout       time.Duration
}

type limiterState struct {
	WindowStart time.Time
	Count       int
}

type actorState struct {
	Limiter      limiterState
	Failures     int
	LockedUntil  time.Time
	LastActivity time.Time
}

type scopeState struct {
	Policy          policy
	PerIP           map[string]*actorState
	GlobalLimiter   limiterState
	GlobalFailures  int
	GlobalLockedTil time.Time
}

type Guard struct {
	mu      sync.Mutex
	pairing scopeState
	auth    scopeState
}

func NewGuard() *Guard {
	return &Guard{
		pairing: scopeState{
			Policy: policy{
				Window:           60 * time.Second,
				PerIPLimit:       12,
				GlobalLimit:      60,
				FailureThreshold: 3,
				BaseLockout:      5 * time.Second,
				MaxLockout:       5 * time.Minute,
			},
			PerIP: map[string]*actorState{},
		},
		auth: scopeState{
			Policy: policy{
				Window:           60 * time.Second,
				PerIPLimit:       60,
				GlobalLimit:      300,
				FailureThreshold: 5,
				BaseLockout:      5 * time.Second,
				MaxLockout:       5 * time.Minute,
			},
			PerIP: map[string]*actorState{},
		},
	}
}

func (g *Guard) Begin(scope Scope, ip string) Decision {
	now := time.Now().UTC()
	ip = normalizeIP(ip)

	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.pick(scope)
	pruneActors(s, now)
	actor := g.getActor(s, ip, now)

	if now.Before(actor.LockedUntil) {
		return Decision{Allowed: false, Code: "ip_locked", Message: "too many failed attempts", RetryAfter: actor.LockedUntil.Sub(now)}
	}
	if now.Before(s.GlobalLockedTil) {
		return Decision{Allowed: false, Code: "global_locked", Message: "service temporarily locked", RetryAfter: s.GlobalLockedTil.Sub(now)}
	}

	if !withinWindow(&actor.Limiter, now, s.Policy.Window) {
		actor.Limiter = limiterState{WindowStart: now}
	}
	if !withinWindow(&s.GlobalLimiter, now, s.Policy.Window) {
		s.GlobalLimiter = limiterState{WindowStart: now}
	}

	if actor.Limiter.Count >= s.Policy.PerIPLimit {
		retry := retryAfterWindow(actor.Limiter, now, s.Policy.Window)
		return Decision{Allowed: false, Code: "rate_limit_ip", Message: "ip rate limit exceeded", RetryAfter: retry}
	}
	if s.GlobalLimiter.Count >= s.Policy.GlobalLimit {
		retry := retryAfterWindow(s.GlobalLimiter, now, s.Policy.Window)
		return Decision{Allowed: false, Code: "rate_limit_global", Message: "global rate limit exceeded", RetryAfter: retry}
	}

	actor.Limiter.Count++
	s.GlobalLimiter.Count++
	actor.LastActivity = now
	return Decision{Allowed: true}
}

func (g *Guard) Success(scope Scope, ip string) {
	now := time.Now().UTC()
	ip = normalizeIP(ip)

	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.pick(scope)
	actor := g.getActor(s, ip, now)
	actor.Failures = 0
	if s.GlobalFailures > 0 {
		s.GlobalFailures--
	}
}

func (g *Guard) Failure(scope Scope, ip string) {
	now := time.Now().UTC()
	ip = normalizeIP(ip)

	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.pick(scope)
	actor := g.getActor(s, ip, now)
	actor.Failures++
	s.GlobalFailures++

	if actor.Failures >= s.Policy.FailureThreshold {
		lock := lockoutDuration(s.Policy, actor.Failures-s.Policy.FailureThreshold)
		until := now.Add(lock)
		if until.After(actor.LockedUntil) {
			actor.LockedUntil = until
		}
	}

	if s.GlobalFailures >= s.Policy.FailureThreshold*3 {
		lock := lockoutDuration(s.Policy, s.GlobalFailures-(s.Policy.FailureThreshold*3))
		s.GlobalLockedTil = now.Add(lock)
		if s.GlobalFailures > 0 {
			s.GlobalFailures = s.Policy.FailureThreshold
		}
	}
}

func (g *Guard) Snapshot() map[string]any {
	now := time.Now().UTC()

	g.mu.Lock()
	defer g.mu.Unlock()

	return map[string]any{
		"pairing": g.scopeSnapshot(g.pick(ScopePairing), now),
		"auth":    g.scopeSnapshot(g.pick(ScopeAuth), now),
	}
}

func (g *Guard) scopeSnapshot(s *scopeState, now time.Time) map[string]any {
	ipLocked := 0
	for _, actor := range s.PerIP {
		if now.Before(actor.LockedUntil) {
			ipLocked++
		}
	}

	globalLockedUntil := any(nil)
	if now.Before(s.GlobalLockedTil) {
		globalLockedUntil = s.GlobalLockedTil.Format(time.RFC3339)
	}

	windowElapsed := 0.0
	if !s.GlobalLimiter.WindowStart.IsZero() {
		windowElapsed = now.Sub(s.GlobalLimiter.WindowStart).Seconds()
		if windowElapsed < 0 {
			windowElapsed = 0
		}
	}

	return map[string]any{
		"policy": map[string]any{
			"window_s":          int(s.Policy.Window.Seconds()),
			"per_ip_limit":      s.Policy.PerIPLimit,
			"global_limit":      s.Policy.GlobalLimit,
			"failure_threshold": s.Policy.FailureThreshold,
			"base_lockout_s":    int(s.Policy.BaseLockout.Seconds()),
			"max_lockout_s":     int(s.Policy.MaxLockout.Seconds()),
		},
		"state": map[string]any{
			"global_window_count":     s.GlobalLimiter.Count,
			"global_window_elapsed_s": windowElapsed,
			"ip_locked_count":         ipLocked,
			"global_locked_until":     globalLockedUntil,
		},
	}
}

func (g *Guard) pick(scope Scope) *scopeState {
	if scope == ScopePairing {
		return &g.pairing
	}
	return &g.auth
}

func (g *Guard) getActor(s *scopeState, ip string, now time.Time) *actorState {
	actor, ok := s.PerIP[ip]
	if !ok {
		actor = &actorState{}
		s.PerIP[ip] = actor
	}
	actor.LastActivity = now
	return actor
}

func withinWindow(limit *limiterState, now time.Time, window time.Duration) bool {
	if limit.WindowStart.IsZero() {
		return false
	}
	return now.Sub(limit.WindowStart) <= window
}

func lockoutDuration(p policy, exponent int) time.Duration {
	if exponent < 0 {
		exponent = 0
	}
	duration := p.BaseLockout << exponent
	if duration > p.MaxLockout {
		return p.MaxLockout
	}
	return duration
}

func retryAfterWindow(limit limiterState, now time.Time, window time.Duration) time.Duration {
	retry := window - now.Sub(limit.WindowStart)
	if retry < 0 {
		return 0
	}
	return retry
}

func normalizeIP(ip string) string {
	trimmed := strings.TrimSpace(ip)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func pruneActors(s *scopeState, now time.Time) {
	if len(s.PerIP) == 0 {
		return
	}
	ttl := s.Policy.Window * 3
	if lockTTL := s.Policy.MaxLockout * 2; lockTTL > ttl {
		ttl = lockTTL
	}
	for ip, actor := range s.PerIP {
		if now.Before(actor.LockedUntil) {
			continue
		}
		if now.Sub(actor.LastActivity) > ttl {
			delete(s.PerIP, ip)
		}
	}
}

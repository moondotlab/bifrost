package providerhealth

import (
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// Config tunes how consecutive credential/quota failures degrade a provider.
type Config struct {
	// FailureThreshold is the number of consecutive counted failures that degrades a
	// provider. Default 3.
	FailureThreshold int
	// BaseCooldown is how long a provider stays degraded after the first time it opens.
	// The cooldown doubles on every re-open, up to MaxCooldown. Default 1 minute.
	BaseCooldown time.Duration
	// MaxCooldown caps the backoff growth. Default 30 minutes.
	MaxCooldown time.Duration
}

// DefaultConfig returns the production defaults.
func DefaultConfig() Config {
	return Config{
		FailureThreshold: 3,
		BaseCooldown:     time.Minute,
		MaxCooldown:      30 * time.Minute,
	}
}

func (c Config) withDefaults() Config {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 3
	}
	if c.BaseCooldown <= 0 {
		c.BaseCooldown = time.Minute
	}
	if c.MaxCooldown <= 0 {
		c.MaxCooldown = 30 * time.Minute
	}
	if c.MaxCooldown < c.BaseCooldown {
		c.MaxCooldown = c.BaseCooldown
	}
	return c
}

type providerState struct {
	consecutiveFailures int
	openUntil           time.Time
	cooldown            time.Duration
}

// Tracker holds per-provider health state for one process. It is in-memory by design:
// a restart simply re-learns provider health within a few requests, and a degraded
// provider rejoins routing purely based on time, so no probe traffic needs to be
// reserved for it.
type Tracker struct {
	mu     sync.Mutex
	config Config
	states map[schemas.ModelProvider]*providerState
	now    func() time.Time
}

// NewTracker builds a tracker with the given config (zero fields fall back to defaults).
func NewTracker(config Config) *Tracker {
	return &Tracker{
		config: config.withDefaults(),
		states: make(map[schemas.ModelProvider]*providerState),
		now:    time.Now,
	}
}

// Default is the process-wide tracker the recording plugin writes to and the routing
// plugins (modelcatalogresolver, governance) read from.
var Default = NewTracker(DefaultConfig())

// CountedFailure reports whether the failure class says something about the provider
// account itself rather than the request: the credential was rejected, or the account
// is out of credit/quota. Anything else — rate limits, transient upstreams, caller
// faults, timeouts — neither counts against the provider nor resets its streak.
func CountedFailure(class schemas.FailureClass) bool {
	return class == schemas.FailureClassCredential || class == schemas.FailureClassQuota
}

// RecordFailure registers one failed attempt. Failures whose class is not counted are
// ignored. It returns true when this call newly degraded the provider.
func (t *Tracker) RecordFailure(provider schemas.ModelProvider, class schemas.FailureClass) bool {
	if provider == "" || !CountedFailure(class) {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.states[provider]
	if st == nil {
		st = &providerState{cooldown: t.config.BaseCooldown}
		t.states[provider] = st
	}
	st.consecutiveFailures++
	now := t.now()
	wasDegraded := now.Before(st.openUntil)
	if wasDegraded || st.consecutiveFailures >= t.config.FailureThreshold {
		// Open, or re-open/extend: a failure that arrives while degraded (or right as the
		// cooldown lapses) proves the provider is still broken, so backoff keeps growing.
		st.openUntil = now.Add(st.cooldown)
		st.cooldown = min(st.cooldown*2, t.config.MaxCooldown)
		return !wasDegraded
	}
	return false
}

// RecordSuccess registers one successful attempt and fully rehabilitates the provider:
// streak, open state and backoff all reset.
func (t *Tracker) RecordSuccess(provider schemas.ModelProvider) {
	if provider == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.states, provider)
}

// IsDegraded reports whether the provider is currently excluded from routing, along
// with how long until it rejoins and how many consecutive counted failures it carries.
// A provider whose cooldown has lapsed is not degraded: it rejoins routing and serves
// as its own probe — the next failure re-opens it immediately because the streak is
// still at or beyond the threshold.
func (t *Tracker) IsDegraded(provider schemas.ModelProvider) (bool, time.Duration, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.states[provider]
	if st == nil {
		return false, 0, 0
	}
	if t.now().Before(st.openUntil) {
		return true, st.openUntil.Sub(t.now()), st.consecutiveFailures
	}
	return false, 0, st.consecutiveFailures
}

// Reset drops all state. Intended for tests.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.states = make(map[schemas.ModelProvider]*providerState)
}

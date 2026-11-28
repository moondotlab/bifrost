package providerhealth

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func newTestTracker() *Tracker {
	tr := NewTracker(Config{FailureThreshold: 3, BaseCooldown: time.Minute, MaxCooldown: 4 * time.Minute})
	return tr
}

func fail(tr *Tracker, p schemas.ModelProvider, class schemas.FailureClass) bool {
	return tr.RecordFailure(p, class)
}

func TestIgnoredClassesDoNotCount(t *testing.T) {
	tr := newTestTracker()
	for range 10 {
		fail(tr, "a6api", schemas.FailureClassRateLimit)
		fail(tr, "a6api", schemas.FailureClassTransient)
		fail(tr, "a6api", schemas.FailureClassCallerFault)
		fail(tr, "a6api", schemas.FailureClassTimeout)
	}
	if degraded, _, _ := tr.IsDegraded("a6api"); degraded {
		t.Fatal("non-credential/quota failures must never degrade a provider")
	}
}

func TestDegradesAfterThresholdConsecutiveFailures(t *testing.T) {
	tr := newTestTracker()
	fail(tr, "a6api", schemas.FailureClassQuota)
	fail(tr, "a6api", schemas.FailureClassQuota)
	if opened := fail(tr, "a6api", schemas.FailureClassQuota); !opened {
		t.Fatal("third consecutive quota failure should degrade the provider")
	}
	degraded, retryIn, failures := tr.IsDegraded("a6api")
	if !degraded {
		t.Fatal("provider should be degraded")
	}
	if retryIn <= 0 || retryIn > time.Minute {
		t.Fatalf("retry-in should be within base cooldown, got %v", retryIn)
	}
	if failures != 3 {
		t.Fatalf("expected 3 consecutive failures, got %d", failures)
	}
}

func TestSuccessResetsStreak(t *testing.T) {
	tr := newTestTracker()
	fail(tr, "a6api", schemas.FailureClassCredential)
	fail(tr, "a6api", schemas.FailureClassCredential)
	tr.RecordSuccess("a6api")
	fail(tr, "a6api", schemas.FailureClassCredential)
	fail(tr, "a6api", schemas.FailureClassCredential)
	if degraded, _, _ := tr.IsDegraded("a6api"); degraded {
		t.Fatal("success must reset the streak; only 2 consecutive failures since")
	}
}

func TestSuccessRehabilitatesDegradedProvider(t *testing.T) {
	tr := newTestTracker()
	for range 3 {
		fail(tr, "a6api", schemas.FailureClassQuota)
	}
	tr.RecordSuccess("a6api")
	degraded, _, failures := tr.IsDegraded("a6api")
	if degraded || failures != 0 {
		t.Fatal("success must clear degradation and streak")
	}
}

func TestCooldownExpiryRejoinsThenReopensOnNextFailure(t *testing.T) {
	tr := newTestTracker()
	now := time.Now()
	tr.now = func() time.Time { return now }
	for range 3 {
		fail(tr, "a6api", schemas.FailureClassQuota)
	}
	// During cooldown: degraded.
	now = now.Add(30 * time.Second)
	if degraded, _, _ := tr.IsDegraded("a6api"); !degraded {
		t.Fatal("should still be degraded inside cooldown")
	}
	// After cooldown lapses: rejoins routing.
	now = now.Add(31 * time.Second)
	if degraded, _, failures := tr.IsDegraded("a6api"); degraded || failures != 3 {
		t.Fatalf("should have rejoined after cooldown with streak intact, degraded=%v", degraded)
	}
	// One more failure re-opens immediately (streak never reset) with doubled backoff.
	if opened := fail(tr, "a6api", schemas.FailureClassQuota); !opened {
		t.Fatal("first failure after cooldown should re-open immediately")
	}
	_, retryIn, _ := tr.IsDegraded("a6api")
	if retryIn > 2*time.Minute || retryIn <= time.Minute {
		t.Fatalf("backoff should have doubled to ~2m, got %v", retryIn)
	}
}

func TestBackoffCapsAtMax(t *testing.T) {
	tr := newTestTracker()
	now := time.Now()
	tr.now = func() time.Time { return now }
	for range 3 {
		fail(tr, "a6api", schemas.FailureClassQuota)
	}
	// Re-open repeatedly: 1m -> 2m -> 4m -> capped at 4m
	for i := range 4 {
		now = now.Add(5 * time.Minute)
		fail(tr, "a6api", schemas.FailureClassQuota)
		_ = i
	}
	_, retryIn, _ := tr.IsDegraded("a6api")
	if retryIn > 4*time.Minute {
		t.Fatalf("backoff must cap at max cooldown, got %v", retryIn)
	}
}

func TestMixedProvidersAreIndependent(t *testing.T) {
	tr := newTestTracker()
	for range 3 {
		fail(tr, "a6api", schemas.FailureClassQuota)
	}
	if degraded, _, _ := tr.IsDegraded("modelflare"); degraded {
		t.Fatal("one provider's failures must not affect another")
	}
}

func TestCredentialAndQuotaBothCount(t *testing.T) {
	tr := newTestTracker()
	fail(tr, "a6api", schemas.FailureClassCredential)
	fail(tr, "a6api", schemas.FailureClassQuota)
	if opened := fail(tr, "a6api", schemas.FailureClassCredential); !opened {
		t.Fatal("credential and quota failures share one streak")
	}
}

package modelcatalogresolver

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/providerhealth"
)

func degradeProvider(p schemas.ModelProvider, failures int) {
	for range failures {
		providerhealth.Default.RecordFailure(p, schemas.FailureClassQuota)
	}
}

func TestFilterDegradedProvidersExcludesDegraded(t *testing.T) {
	providerhealth.Default.Reset()
	defer providerhealth.Default.Reset()
	degradeProvider("a6api", 3)

	got := filterDegradedProviders(nil, []schemas.ModelProvider{"a6api", "modelflare"}, "claude-sonnet-5-5")
	if len(got) != 1 || got[0] != "modelflare" {
		t.Fatalf("degraded a6api should be excluded, got %v", got)
	}
}

func TestFilterDegradedProvidersKeepsAllWhenAllDegraded(t *testing.T) {
	providerhealth.Default.Reset()
	defer providerhealth.Default.Reset()
	degradeProvider("a6api", 3)
	degradeProvider("modelflare", 3)

	got := filterDegradedProviders(nil, []schemas.ModelProvider{"a6api", "modelflare"}, "claude-sonnet-5-5")
	if len(got) != 2 {
		t.Fatalf("when every candidate is degraded they must stay as last resort, got %v", got)
	}
}

func TestFilterDegradedProvidersNoopWhenHealthy(t *testing.T) {
	providerhealth.Default.Reset()
	defer providerhealth.Default.Reset()

	got := filterDegradedProviders(nil, []schemas.ModelProvider{"a6api", "modelflare"}, "claude-sonnet-5-5")
	if len(got) != 2 {
		t.Fatalf("healthy candidates must pass through untouched, got %v", got)
	}
}

func TestFilterDegradedProvidersRecoversAfterSuccess(t *testing.T) {
	providerhealth.Default.Reset()
	defer providerhealth.Default.Reset()
	degradeProvider("a6api", 3)
	providerhealth.Default.RecordSuccess("a6api")

	got := filterDegradedProviders(nil, []schemas.ModelProvider{"a6api", "modelflare"}, "claude-sonnet-5-5")
	if len(got) != 2 {
		t.Fatalf("a provider that served successfully must rejoin routing, got %v", got)
	}
}

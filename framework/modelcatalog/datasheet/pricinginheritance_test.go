package datasheet

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inheritanceRowSpec struct {
	model, provider, mode string
	input                 float64
}

func inheritanceStore(rows ...inheritanceRowSpec) *Store {
	entries := map[string]configstoreTables.TableModelPricing{}
	for _, r := range rows {
		p := chatPricing(r.input, r.input)
		p.Model, p.Provider, p.Mode = r.model, r.provider, r.mode
		entries[makeKeyNormalized(r.model, r.provider, r.mode)] = p
	}
	return testStoreWithPricing(entries)
}

// A custom provider serving a first-party model under its canonical name
// inherits the first-party row instead of pricing at zero — the modelflare
// deployment shape this feature exists for.
func TestPricingInheritance_CustomProviderInheritsFirstPartyRow(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"gpt-6.2-sol", "openai", "responses", 5},
	)

	for _, requestType := range []schemas.RequestType{schemas.ResponsesRequest, schemas.ChatCompletionRequest} {
		pricing, ok := s.getBasePricing("gpt-6.2-sol", "modelflare", requestType)
		require.True(t, ok, "expected modelflare/gpt-6.2-sol to inherit pricing")
		require.NotNil(t, pricing.InputCostPerToken)
		assert.Equal(t, 5.0, *pricing.InputCostPerToken)
		assert.Equal(t, "openai", pricing.Provider, "the row keeps its source provider for attribution")
	}
}

// A provider's own row always wins over inheritance.
func TestPricingInheritance_OwnRowWins(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"gpt-6.2-sol", "openai", "chat", 5},
		inheritanceRowSpec{"gpt-6.2-sol", "modelflare", "chat", 9},
	)

	pricing, ok := s.getBasePricing("gpt-6.2-sol", "modelflare", schemas.ChatCompletionRequest)
	require.True(t, ok)
	require.NotNil(t, pricing.InputCostPerToken)
	assert.Equal(t, 9.0, *pricing.InputCostPerToken)
	assert.Equal(t, "modelflare", pricing.Provider)
}

// The canonical first-party provider is preferred even when another provider
// sorts before it alphabetically (azure < openai).
func TestPricingInheritance_CanonicalProviderBeatsAlphabetical(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"gpt-6.2-sol", "azure", "chat", 3},
		inheritanceRowSpec{"gpt-6.2-sol", "openai", "chat", 5},
	)

	pricing, ok := s.getBasePricing("gpt-6.2-sol", "modelflare", schemas.ChatCompletionRequest)
	require.True(t, ok)
	require.NotNil(t, pricing.InputCostPerToken)
	assert.Equal(t, 5.0, *pricing.InputCostPerToken)
	assert.Equal(t, "openai", pricing.Provider)
}

// Without a canonical family match, the alphabetically first provider holding
// a row answers, deterministically.
func TestPricingInheritance_NoCanonicalFallsToAlphabetical(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"llama-4-scout", "together_ai", "chat", 3},
		inheritanceRowSpec{"llama-4-scout", "openrouter", "chat", 7},
	)

	pricing, ok := s.getBasePricing("llama-4-scout", "modelflare", schemas.ChatCompletionRequest)
	require.True(t, ok)
	require.NotNil(t, pricing.InputCostPerToken)
	// openrouter < together_ai alphabetically.
	assert.Equal(t, 7.0, *pricing.InputCostPerToken)
	assert.Equal(t, "openrouter", pricing.Provider)
}

// A model priced under only one of the chat/responses modes prices requests in
// the other, mirroring the same-provider counterpart fallback.
func TestPricingInheritance_CounterpartModeAcrossProviders(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"gpt-6.2-sol", "openai", "responses", 5},
	)

	pricing, ok := s.getBasePricing("gpt-6.2-sol", "modelflare", schemas.ChatCompletionRequest)
	require.True(t, ok, "chat request should inherit the openai responses row")
	require.NotNil(t, pricing.InputCostPerToken)
	assert.Equal(t, 5.0, *pricing.InputCostPerToken)
}

// Bedrock rows carry the vendor namespace; a bare model name still inherits
// them when no first-party row exists.
func TestPricingInheritance_BedrockVendorPrefixedRow(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"anthropic.claude-sonnet-4", "bedrock", "chat", 4},
	)

	pricing, ok := s.getBasePricing("claude-sonnet-4", "modelflare", schemas.ChatCompletionRequest)
	require.True(t, ok, "expected inheritance from the bedrock namespaced row")
	require.NotNil(t, pricing.InputCostPerToken)
	assert.Equal(t, 4.0, *pricing.InputCostPerToken)
	assert.Equal(t, "bedrock", pricing.Provider)
}

// The first-party row still beats Bedrock's namespaced copy for the same model.
func TestPricingInheritance_FirstPartyBeatsBedrock(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"anthropic.claude-sonnet-4", "bedrock", "chat", 4},
		inheritanceRowSpec{"claude-sonnet-4", "anthropic", "chat", 6},
	)

	pricing, ok := s.getBasePricing("claude-sonnet-4", "modelflare", schemas.ChatCompletionRequest)
	require.True(t, ok)
	require.NotNil(t, pricing.InputCostPerToken)
	assert.Equal(t, 6.0, *pricing.InputCostPerToken)
	assert.Equal(t, "anthropic", pricing.Provider)
}

// Inheritance reads through the normalized key, so casing never matters.
func TestPricingInheritance_CaseInsensitive(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"GPT-6.2-Sol", "OpenAI", "chat", 5},
	)

	pricing, ok := s.getBasePricing("gpt-6.2-sol", "modelflare", schemas.ChatCompletionRequest)
	require.True(t, ok)
	require.NotNil(t, pricing.InputCostPerToken)
	assert.Equal(t, 5.0, *pricing.InputCostPerToken)
}

// A model no provider prices stays unpriced: inheritance never invents rows.
func TestPricingInheritance_NoRowAnywhere(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"gpt-6.2-sol", "openai", "chat", 5},
	)

	_, ok := s.getBasePricing("some-internal-model", "modelflare", schemas.ChatCompletionRequest)
	assert.False(t, ok)
}

// End to end through resolvePricing: the inherited base row is what overrides
// patch, so a scoped override still wins over inherited pricing.
func TestPricingInheritance_OverridePatchesInheritedBase(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"gpt-6.2-sol", "openai", "chat", 5},
	)
	providerID := "modelflare"
	require.NoError(t, s.SetOverrides([]configstoreTables.TablePricingOverride{
		{
			ID:               "modelflare-override",
			ScopeKind:        string(ScopeKindProvider),
			ProviderID:       &providerID,
			MatchType:        string(MatchTypeExact),
			Pattern:          "gpt-6.2-sol",
			RequestTypes:     []schemas.RequestType{schemas.ChatCompletionRequest},
			PricingPatchJSON: `{"input_cost_per_token":42}`,
		},
	}))

	pricing := s.resolvePricing(schemas.RoutingInfo{Provider: "modelflare", Model: "gpt-6.2-sol"}, schemas.ChatCompletionRequest, LookupScopes{Provider: "modelflare"})
	require.NotNil(t, pricing)
	require.NotNil(t, pricing.InputCostPerToken)
	assert.Equal(t, 42.0, *pricing.InputCostPerToken, "the provider-scoped override patches the inherited base")
	require.NotNil(t, pricing.OutputCostPerToken)
	assert.Equal(t, 5.0, *pricing.OutputCostPerToken, "unpatched fields keep the inherited row's rates")
}

// resolvePricing end to end without overrides: the custom provider's request
// prices instead of returning nil.
func TestResolvePricing_InheritsAcrossProviders(t *testing.T) {
	s := inheritanceStore(
		inheritanceRowSpec{"gpt-6.2-sol", "openai", "responses", 5},
	)

	pricing := s.resolvePricing(schemas.RoutingInfo{Provider: "modelflare", Model: "gpt-6.2-sol"}, schemas.ResponsesRequest, LookupScopes{Provider: "modelflare"})
	require.NotNil(t, pricing, "resolvePricing should no longer return nil for modelflare/gpt-6.2-sol")
	require.NotNil(t, pricing.InputCostPerToken)
	assert.Equal(t, 5.0, *pricing.InputCostPerToken)
}

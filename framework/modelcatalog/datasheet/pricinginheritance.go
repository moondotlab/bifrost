package datasheet

import (
	"slices"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

// Cross-provider pricing inheritance.
//
// Custom providers (relay or proxy deployments such as modelflare) serve
// first-party models under the same model name but rarely have datasheet rows
// of their own, so every request through them priced at zero: cost tracking,
// budgets and reconciliation all go blind. An approximate price is strictly
// better than none — the mismatch is visible in dashboards, a zero is not —
// so when every same-provider fallback in getBasePricing misses, the model is
// priced from another provider's row for the same model name.
//
// Candidate order is deterministic: the canonical first-party provider
// inferred from the model family first, then every remaining provider with a
// row for the model in alphabetical order. The returned row keeps its own
// Provider, so inherited pricing stays attributable downstream.

// inheritProviderPricingUnsafe probes other providers' rows for the same model
// name. It runs inside getBasePricing, so the caller already holds s.mu (read).
// provider is the catalog-normalized request provider, whose rows have all
// been probed already and which is skipped here.
func (s *Store) inheritProviderPricingUnsafe(model, provider, mode, fallbackMode string, hasFallbackMode bool) (*configstoreTables.TableModelPricing, bool) {
	providers := s.pricingProvidersByModel[strings.ToLower(model)]
	if len(providers) == 0 {
		return nil, false
	}
	ordered := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, canonical := range canonicalPricingProviders(model) {
		if slices.Contains(providers, canonical) {
			if _, dup := seen[canonical]; !dup {
				seen[canonical] = struct{}{}
				ordered = append(ordered, canonical)
			}
		}
	}
	for _, p := range providers {
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			ordered = append(ordered, p)
		}
	}
	vendorPrefix := bedrockVendorPrefix(model)
	for _, candidate := range ordered {
		if candidate == provider {
			continue
		}
		if pricing, ok := s.pricingData[makeKeyNormalized(model, candidate, mode)]; ok {
			s.logger.Debug("pricing inherited for model %s from provider %s in %s mode", model, candidate, mode)
			return &pricing, true
		}
		if hasFallbackMode {
			if pricing, ok := s.pricingData[makeKeyNormalized(model, candidate, fallbackMode)]; ok {
				s.logger.Debug("pricing inherited for model %s from provider %s in %s mode", model, candidate, fallbackMode)
				return &pricing, true
			}
		}
		// Bedrock rows carry a vendor namespace ("anthropic.claude-*"), so a
		// bare model name only matches there when prefixed.
		if candidate == string(schemas.Bedrock) && vendorPrefix != "" {
			prefixed := vendorPrefix + model
			if pricing, ok := s.pricingData[makeKeyNormalized(prefixed, candidate, mode)]; ok {
				s.logger.Debug("pricing inherited for model %s from provider %s row %s in %s mode", model, candidate, prefixed, mode)
				return &pricing, true
			}
			if hasFallbackMode {
				if pricing, ok := s.pricingData[makeKeyNormalized(prefixed, candidate, fallbackMode)]; ok {
					s.logger.Debug("pricing inherited for model %s from provider %s row %s in %s mode", model, candidate, prefixed, fallbackMode)
					return &pricing, true
				}
			}
		}
	}
	return nil, false
}

// canonicalPricingProviders names the first-party provider a model family is
// published by, in the order its rows should be preferred for inheritance.
// Model families without a first-party API in the catalog (llama, mistral,
// qwen, …) get no canonical candidate and rely on alphabetical order alone.
func canonicalPricingProviders(model string) []string {
	family := strings.ToLower(model)
	switch {
	case schemas.IsAnthropicModel(family):
		return []string{string(schemas.Anthropic)}
	case schemas.IsOpenAIModel(family):
		return []string{string(schemas.OpenAI), string(schemas.Azure)}
	case schemas.IsGeminiModel(family) || schemas.IsGemmaModel(family):
		return []string{string(schemas.Gemini), string(schemas.Vertex)}
	case schemas.IsGrokModel(family):
		return []string{string(schemas.XAI)}
	case schemas.IsDeepSeekModel(family):
		return []string{string(schemas.DeepSeek)}
	}
	return nil
}

// bedrockVendorPrefix mirrors the vendor-namespace inference getBasePricing
// applies to same-provider Bedrock lookups, reused for inherited rows filed
// under Bedrock's namespaced spelling.
func bedrockVendorPrefix(model string) string {
	family := strings.ToLower(model)
	switch {
	case !strings.Contains(family, "anthropic.") && schemas.IsAnthropicModel(family):
		return "anthropic."
	case !strings.Contains(family, "openai.") && schemas.IsOpenAIModel(family):
		return "openai."
	case !strings.Contains(family, "google.") && (schemas.IsGemmaModel(family) || schemas.IsGeminiModel(family)):
		return "google."
	case !strings.Contains(family, "xai.") && schemas.IsGrokModel(family):
		return "xai."
	}
	return ""
}

// rebuildPricingProvidersByModelUnsafe indexes every model name onto the
// sorted set of providers holding a pricing row for it, so inheritance probes
// never scan the pricing map on the request path. Bedrock's vendor-namespaced
// rows ("anthropic.claude-*") index under the bare model name too, since that
// is the spelling requests carry. Caller MUST hold s.mu's write lock.
func (s *Store) rebuildPricingProvidersByModelUnsafe() {
	byModel := make(map[string]map[string]struct{})
	for _, row := range s.pricingData {
		model := strings.ToLower(row.Model)
		provider := strings.ToLower(row.Provider)
		if model == "" || provider == "" {
			continue
		}
		names := []string{model}
		if bare, ok := strings.CutPrefix(model, "anthropic."); ok {
			names = append(names, bare)
		} else if bare, ok = strings.CutPrefix(model, "openai."); ok {
			names = append(names, bare)
		} else if bare, ok = strings.CutPrefix(model, "google."); ok {
			names = append(names, bare)
		} else if bare, ok = strings.CutPrefix(model, "xai."); ok {
			names = append(names, bare)
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			if byModel[name] == nil {
				byModel[name] = make(map[string]struct{})
			}
			byModel[name][provider] = struct{}{}
		}
	}
	s.pricingProvidersByModel = make(map[string][]string, len(byModel))
	for model, set := range byModel {
		providers := make([]string, 0, len(set))
		for provider := range set {
			providers = append(providers, provider)
		}
		slices.Sort(providers)
		s.pricingProvidersByModel[model] = providers
	}
}

package datasheet

import (
	"cmp"
	"slices"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

type parameterModelMetadata struct {
	modes     []string
	endpoints []string
}

// ModelMetadata returns the modes and explicit parameter-sheet endpoints for
// a provider's model. Exact model rows take precedence over base and family
// rows. Returned slices are owned by the caller.
func (s *Store) ModelMetadata(model string, provider schemas.ModelProvider) (modes, endpoints []string) {
	if model == "" || provider == "" {
		return nil, nil
	}
	catalogProvider := metadataProvider(string(provider))
	model = stripMetadataProvider(model, catalogProvider)
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := makeKeyNormalized(model, catalogProvider, "")
	modes = s.modelModes[key]
	params := s.parameterMetadata[key]
	hasExactModes := len(modes) > 0 || len(params.modes) > 0
	if !hasExactModes || (len(params.endpoints) == 0 && len(s.parameterMetadata) > 0) {
		base := schemas.BaseModelName(model)
		baseKey := makeKeyNormalized(base, catalogProvider, "")
		baseParams := s.parameterMetadata[baseKey]
		if !hasExactModes {
			modes = s.modelModes[baseKey]
			params.modes = baseParams.modes
			if len(modes) == 0 && len(params.modes) == 0 {
				modes = s.familyModes[baseKey]
			}
		}
		if len(params.endpoints) == 0 {
			params.endpoints = baseParams.endpoints
		}
	}
	pricingModeCount := len(modes)
	modes = slices.Clone(modes)
	for _, mode := range params.modes {
		if !slices.ContainsFunc(modes, func(existing string) bool { return strings.EqualFold(existing, mode) }) {
			modes = append(modes, mode)
		}
	}
	if len(modes) > pricingModeCount {
		modes = sortCapabilityModes(modes)
	}
	return modes, slices.Clone(params.endpoints)
}

// rebuildModelModesUnsafe indexes the effective rows, including overrides.
// Caller holds mu. The first mode in each slice is the preferred mode.
func (s *Store) rebuildModelModesUnsafe() {
	s.modelModes = make(map[string][]string)
	s.familyModes = make(map[string][]string)
	for _, row := range s.pricingData {
		if row.Mode == "" {
			continue
		}
		provider := metadataProvider(row.Provider)
		model := stripMetadataProvider(row.Model, provider)
		exact := makeKeyNormalized(model, provider, "")
		s.modelModes[exact] = append(s.modelModes[exact], row.Mode)
		base := stripMetadataProvider(row.BaseModel, provider)
		if base == "" {
			base = schemas.BaseModelName(model)
		}
		if base != "" {
			family := makeKeyNormalized(base, provider, "")
			s.familyModes[family] = append(s.familyModes[family], row.Mode)
		}
	}
	for _, index := range []map[string][]string{s.modelModes, s.familyModes} {
		for key, modes := range index {
			index[key] = sortCapabilityModes(modes)
		}
	}
}

func sortCapabilityModes(modes []string) []string {
	slices.SortFunc(modes, func(a, b string) int {
		if order := cmp.Compare(modePriority(a), modePriority(b)); order != 0 {
			return order
		}
		if order := strings.Compare(strings.ToLower(a), strings.ToLower(b)); order != 0 {
			return order
		}
		return strings.Compare(a, b)
	})
	return slices.CompactFunc(modes, strings.EqualFold)
}

var preferredCapabilityModes = [...]string{"chat", "responses", "completion"}

func modePriority(mode string) int {
	if priority := slices.Index(preferredCapabilityModes[:], strings.ToLower(mode)); priority >= 0 {
		return priority
	}
	return len(preferredCapabilityModes)
}

// Apply exact catalog aliases. The pricing normalizer also folds provider
// families by substring, which would mix capabilities of distinct providers
// such as Bedrock and Bedrock Mantle.
func metadataProvider(provider string) string {
	provider = strings.ToLower(provider)
	switch provider {
	case "together", "vertex_ai", "google-vertex", "runwayml", "fireworks_ai",
		"github_copilot", "volcano", "volcengine_ark", string(schemas.Zhipu):
		return normalizeProvider(provider)
	default:
		return provider
	}
}

func stripMetadataProvider(model, provider string) string {
	if prefix, bare := schemas.ParseModelString(model, ""); prefix != "" && metadataProvider(string(prefix)) == provider {
		return bare
	}
	return model
}

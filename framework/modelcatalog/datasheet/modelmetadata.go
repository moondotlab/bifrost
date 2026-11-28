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

type modelMetadataKey struct {
	model    string
	provider string
}

func metadataKey(model, provider string) modelMetadataKey {
	return modelMetadataKey{strings.ToLower(stripMetadataProvider(model, provider)), provider}
}

// ModelMetadata returns provider-scoped modes and explicit endpoints, with
// exact model rows taking precedence over base and family rows.
func (s *Store) ModelMetadata(model string, provider schemas.ModelProvider) (modes, endpoints []string) {
	modes, endpoints, _ = s.ModelMetadataForCandidates(provider, false, model)
	return modes, endpoints
}

// ModelMetadataForCandidates resolves ordered names for models API enrichment.
// For custom providers, every scoped exact candidate precedes name consensus.
// Returned slices are owned by the caller. The scoped result reports whether
// the listed provider supplied catalog capability metadata.
func (s *Store) ModelMetadataForCandidates(provider schemas.ModelProvider, allowConsensus bool, candidates ...string) (modes, endpoints []string, scoped bool) {
	if provider == "" {
		return nil, nil, false
	}
	catalogProvider := metadataProvider(string(provider))
	s.mu.RLock()
	defer s.mu.RUnlock()

	if allowConsensus {
		if modes, endpoints, exact := s.scopedCandidateMetadataUnsafe(catalogProvider, candidates, true); exact {
			return modes, endpoints, true
		}
	}
	modes, endpoints, _ = s.scopedCandidateMetadataUnsafe(catalogProvider, candidates, false)
	scoped = len(modes) > 0 || len(endpoints) > 0
	if allowConsensus && (len(modes) == 0 || len(endpoints) == 0) {
		for _, candidate := range candidates {
			_, name := schemas.ParseModelString(candidate, "")
			metadata := s.modelConsensus[strings.ToLower(name)]
			if len(modes) == 0 {
				modes = slices.Clone(metadata.modes)
			}
			if len(endpoints) == 0 {
				endpoints = slices.Clone(metadata.endpoints)
			}
			if len(modes) > 0 && len(endpoints) > 0 {
				break
			}
		}
	}
	return modes, endpoints, scoped
}

func (s *Store) scopedCandidateMetadataUnsafe(provider string, candidates []string, exactOnly bool) (modes, endpoints []string, found bool) {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		key := metadataKey(candidate, provider)
		if exactOnly && !s.hasExactMetadataUnsafe(key) {
			_, name := schemas.ParseModelString(candidate, "")
			key.model = strings.ToLower(name)
			if !s.hasExactMetadataUnsafe(key) {
				continue
			}
		}
		found = true
		candidateModes, candidateEndpoints := s.scopedModelMetadataUnsafe(key)
		if len(modes) == 0 {
			modes = candidateModes
		}
		if len(endpoints) == 0 {
			endpoints = candidateEndpoints
		}
		if len(modes) > 0 && len(endpoints) > 0 {
			break
		}
	}
	return modes, endpoints, found
}

func (s *Store) hasExactMetadataUnsafe(key modelMetadataKey) bool {
	_, pricing := s.modelModes[key]
	_, parameters := s.parameterMetadata[key]
	return pricing || parameters
}

func (s *Store) scopedModelMetadataUnsafe(key modelMetadataKey) (modes, endpoints []string) {
	modes = s.modelModes[key]
	params := s.parameterMetadata[key]
	hasExactModes := len(modes) > 0 || len(params.modes) > 0
	if !hasExactModes || (len(params.endpoints) == 0 && len(s.parameterMetadata) > 0) {
		baseKey := modelMetadataKey{schemas.BaseModelName(key.model), key.provider}
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
	return mergeCapabilityModes(modes, params.modes), slices.Clone(params.endpoints)
}

func mergeCapabilityModes(pricing, params []string) []string {
	modes := slices.Clone(pricing)
	for _, mode := range params {
		if !slices.ContainsFunc(modes, func(existing string) bool { return strings.EqualFold(existing, mode) }) {
			modes = append(modes, mode)
		}
	}
	if len(modes) > len(pricing) {
		modes = sortCapabilityModes(modes)
	}
	return modes
}

// rebuildModelModesUnsafe indexes the effective rows, including overrides.
// Caller holds mu. The first mode in each slice is the preferred mode.
func (s *Store) rebuildModelModesUnsafe() {
	s.modelModes = make(map[modelMetadataKey][]string)
	s.familyModes = make(map[modelMetadataKey][]string)
	for _, row := range s.pricingData {
		provider := metadataProvider(row.Provider)
		exact := metadataKey(row.Model, provider)
		if exact.model == "" || provider == "" {
			continue
		}
		if row.Mode == "" {
			if _, ok := s.modelModes[exact]; !ok {
				s.modelModes[exact] = nil
			}
			continue
		}
		s.modelModes[exact] = append(s.modelModes[exact], row.Mode)
		base := stripMetadataProvider(row.BaseModel, provider)
		if base == "" {
			base = schemas.BaseModelName(exact.model)
		}
		if base != "" {
			family := modelMetadataKey{strings.ToLower(base), provider}
			s.familyModes[family] = append(s.familyModes[family], row.Mode)
		}
	}
	for _, index := range []map[modelMetadataKey][]string{s.modelModes, s.familyModes} {
		for key, modes := range index {
			index[key] = sortCapabilityModes(modes)
		}
	}
	s.rebuildPricingProvidersByModelUnsafe()
	s.rebuildModelConsensusUnsafe()
}

// rebuildModelConsensusUnsafe prefers chat among exact-name modes and otherwise
// intersects distinct provider mode sets. Explicit endpoint consensus is independent.
// Caller holds mu. Family indexes are excluded.
func (s *Store) rebuildModelConsensusUnsafe() {
	type consensus struct {
		parameterModelMetadata
		hasChat           bool
		providers         int
		endpointProviders int
	}
	byName := make(map[string]consensus)
	add := func(name string, modes, endpoints []string) {
		if name == "" || (len(modes) == 0 && len(endpoints) == 0) {
			return
		}
		entry := byName[name]
		entry.hasChat = entry.hasChat || slices.ContainsFunc(modes, func(mode string) bool {
			return strings.EqualFold(mode, "chat")
		})
		if !entry.hasChat {
			if entry.providers == 0 {
				for _, mode := range modes {
					entry.modes = append(entry.modes, strings.ToLower(mode))
				}
			} else {
				entry.modes = slices.DeleteFunc(entry.modes, func(mode string) bool {
					return !slices.ContainsFunc(modes, func(other string) bool { return strings.EqualFold(mode, other) })
				})
			}
		}
		entry.providers++
		if len(endpoints) > 0 {
			if entry.endpointProviders == 0 {
				entry.endpoints = slices.Clone(endpoints)
			} else {
				entry.endpoints = slices.DeleteFunc(entry.endpoints, func(endpoint string) bool {
					return !slices.Contains(endpoints, endpoint)
				})
			}
			entry.endpointProviders++
		}
		byName[name] = entry
	}
	for key, modes := range s.modelModes {
		params := s.parameterMetadata[key]
		add(key.model, mergeCapabilityModes(modes, params.modes), params.endpoints)
	}
	for key, params := range s.parameterMetadata {
		if _, ok := s.modelModes[key]; !ok {
			add(key.model, params.modes, params.endpoints)
		}
	}

	s.modelConsensus = make(map[string]parameterModelMetadata, len(byName))
	for name, entry := range byName {
		if entry.endpointProviders < 2 {
			entry.endpoints = nil
		}
		if entry.hasChat {
			entry.modes = []string{"chat"}
		} else {
			entry.modes = sortCapabilityModes(entry.modes)
		}
		if len(entry.modes) > 0 || len(entry.endpoints) > 0 {
			s.modelConsensus[name] = entry.parameterModelMetadata
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
	if prefix, bare, ok := strings.Cut(model, "/"); ok && metadataProvider(prefix) == provider {
		return bare
	}
	return model
}

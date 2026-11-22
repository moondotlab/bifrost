package modelcatalog

import (
	"slices"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// Each pricing mode describes its primary gateway operation. Other operations
// can share a pricing mode without sharing that capability.
var modelCapabilityByMode = map[string]struct {
	endpoint string
	method   schemas.RequestType
}{
	"chat":                {"/v1/chat/completions", schemas.ChatCompletionRequest},
	"responses":           {"/v1/responses", schemas.ResponsesRequest},
	"completion":          {"/v1/completions", schemas.TextCompletionRequest},
	"embedding":           {"/v1/embeddings", schemas.EmbeddingRequest},
	"rerank":              {"/v1/rerank", schemas.RerankRequest},
	"decisions":           {"/v1/decisions", schemas.DecisionRequest},
	"audio_speech":        {"/v1/audio/speech", schemas.SpeechRequest},
	"audio_transcription": {"/v1/audio/transcriptions", schemas.TranscriptionRequest},
	"image_generation":    {"/v1/images/generations", schemas.ImageGenerationRequest},
	"image_edit":          {"/v1/images/edits", schemas.ImageEditRequest},
	"video_generation":    {"/v1/videos", schemas.VideoGenerationRequest},
	"ocr":                 {"/v1/ocr", schemas.OCRRequest},
	"container_create":    {"/v1/containers", schemas.ContainerCreateRequest},
	"live":                {"/v1/live/sessions", schemas.LiveRequest},
}

var modelMethodByEndpoint = func() map[string]schemas.RequestType {
	methods := map[string]schemas.RequestType{
		"/v1/images/variations":       schemas.ImageVariationRequest,
		"/v1/videos/edits":            schemas.VideoEditRequest,
		"/v1/videos/{video_id}/remix": schemas.VideoRemixRequest,
		"/v1/responses/compact":       schemas.CompactionRequest,
		"/v1/responses/input_tokens":  schemas.CountTokensRequest,
	}
	for _, capability := range modelCapabilityByMode {
		methods[capability.endpoint] = capability.method
	}
	return methods
}()

func (mc *ModelCatalog) enrichModelCapabilities(model *schemas.Model, name string, provider schemas.ModelProvider) bool {
	if provider == "" {
		return false
	}
	modes, endpoints, scoped := mc.modelMetadata(name, model.Alias, provider)
	if model.Mode == "" && len(modes) > 0 {
		model.Mode = modes[0]
	}
	// Clone on the first addition to each list so provider-owned slices remain
	// untouched. Subsequent additions can reuse the response's backing array.
	endpointsOwned, methodsOwned := false, false
	methods := model.SupportedMethods
	for _, mode := range modes {
		if capability, ok := modelCapabilityByMode[strings.ToLower(mode)]; ok {
			model.SupportedEndpoints = appendCapability(model.SupportedEndpoints, capability.endpoint, &endpointsOwned)
			methods = appendCapability(methods, string(capability.method), &methodsOwned)
		}
	}
	for _, endpoint := range endpoints {
		if endpoint != "" {
			model.SupportedEndpoints = appendCapability(model.SupportedEndpoints, endpoint, &endpointsOwned)
			if method, ok := modelMethodByEndpoint[endpoint]; ok {
				methods = appendCapability(methods, string(method), &methodsOwned)
			}
		}
	}
	model.SetEnrichedSupportedMethods(methods)
	return scoped
}

func appendCapability(values []string, value string, owned *bool) []string {
	if slices.Contains(values, value) {
		return values
	}
	if !*owned {
		values = values[:len(values):len(values)]
		*owned = true
	}
	return append(values, value)
}

func (mc *ModelCatalog) modelMetadata(name string, modelAlias *string, provider schemas.ModelProvider) (modes, endpoints []string, scoped bool) {
	// Canonical name → wire ID → listed name, repeated for a provider-reported alias.
	var candidates [6]string
	count := mc.metadataCandidates(candidates[:], name, provider)
	if modelAlias != nil {
		count += mc.metadataCandidates(candidates[count:], *modelAlias, provider)
	}
	custom := !slices.Contains(schemas.StandardProviders, provider) && schemas.IsKnownProvider(string(provider))
	return mc.datasheet.ModelMetadataForCandidates(provider, custom, candidates[:count]...)
}

func (mc *ModelCatalog) metadataCandidates(candidates []string, name string, provider schemas.ModelProvider) int {
	if mc.keyconf != nil {
		if alias, ok := mc.keyconf.ResolveAlias(provider, name); ok {
			if alias.Config.ModelName != nil {
				candidates[0] = *alias.Config.ModelName
			}
			candidates[1] = alias.Config.ModelID
			candidates[2] = name
			return 3
		}
	}
	candidates[0] = name
	return 1
}

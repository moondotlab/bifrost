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

func (mc *ModelCatalog) enrichModelCapabilities(model *schemas.Model, name string, provider schemas.ModelProvider) {
	if provider == "" {
		return
	}
	modes, endpoints := mc.modelMetadata(name, provider)
	if (len(modes) == 0 || len(endpoints) == 0) && model.Alias != nil {
		aliasModes, aliasEndpoints := mc.modelMetadata(*model.Alias, provider)
		if len(modes) == 0 {
			modes = aliasModes
		}
		if len(endpoints) == 0 {
			endpoints = aliasEndpoints
		}
	}
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

func (mc *ModelCatalog) modelMetadata(name string, provider schemas.ModelProvider) (modes, endpoints []string) {
	if mc.keyconf == nil {
		return mc.datasheet.ModelMetadata(name, provider)
	}
	alias, ok := mc.keyconf.ResolveAlias(provider, name)
	if !ok {
		return mc.datasheet.ModelMetadata(name, provider)
	}

	// Same canonical name → wire ID → alias priority as capability lookup.
	var canonicalName string
	if alias.Config.ModelName != nil {
		canonicalName = *alias.Config.ModelName
	}
	for _, candidate := range [...]string{canonicalName, alias.Config.ModelID, name} {
		candidateModes, candidateEndpoints := mc.datasheet.ModelMetadata(candidate, provider)
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
	return modes, endpoints
}

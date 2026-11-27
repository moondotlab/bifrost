package compat

import (
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// uniqueLearnedProvider keeps tests independent of the package-level learned
// registry and of each other: entries live 24h, so each test uses its own pair.
func uniqueLearnedProvider(name string) schemas.ModelProvider {
	return schemas.ModelProvider("learned-" + name)
}

func TestUnsupportedParamFromError(t *testing.T) {
	mkErr := func(isBifrost bool, param any, message string) *schemas.BifrostError {
		return &schemas.BifrostError{
			IsBifrostError: isBifrost,
			Error:          &schemas.ErrorField{Message: message, Param: param},
		}
	}
	tests := []struct {
		name string
		err  *schemas.BifrostError
		want string
	}{
		{
			name: "modelflare style message",
			err:  mkErr(false, nil, "prompt_cache_breakpoint is not supported on this model (request id: abc)"),
			want: "prompt_cache_breakpoint",
		},
		{
			name: "openai unrecognized argument",
			err:  mkErr(false, nil, "Unrecognized request argument supplied: temperature"),
			want: "temperature",
		},
		{
			name: "unsupported parameter phrasing",
			err:  mkErr(false, nil, "Unsupported parameter: 'top_p' is not supported with this model."),
			want: "top_p",
		},
		{
			name: "error param field wins",
			err:  mkErr(false, "seed", "invalid request"),
			want: "seed",
		},
		{
			name: "nested param path collapses to leaf",
			err:  mkErr(false, "input[3].content[0].prompt_cache_breakpoint", "bad field"),
			want: "prompt_cache_breakpoint",
		},
		{
			name: "bifrost internal error ignored",
			err:  mkErr(true, nil, "prompt_cache_breakpoint is not supported on this model"),
			want: "",
		},
		{
			name: "non-param error ignored",
			err:  mkErr(false, nil, "rate limit exceeded, retry later"),
			want: "",
		},
		{
			name: "unstrippable field ignored",
			err:  mkErr(false, nil, "model is not supported on this endpoint"),
			want: "",
		},
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := schemas.UnsupportedParamFromError(tt.err); got != tt.want {
				t.Fatalf("UnsupportedParamFromError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLearnedUnsupportedFor(t *testing.T) {
	provider := uniqueLearnedProvider("store")
	if got := schemas.LearnedUnsupportedFor(provider, "m"); got.HasWork() {
		t.Fatal("expected no learned params before learning")
	}

	if !schemas.LearnUnsupportedParam(provider, "m", "prompt_cache_breakpoint") {
		t.Fatal("expected first learn to report a change")
	}
	if !schemas.LearnUnsupportedParam(provider, "m", "temperature") {
		t.Fatal("expected temperature learn to report a change")
	}
	if schemas.LearnUnsupportedParam(provider, "m", "model") {
		t.Fatal("unstrippable param should not be learned")
	}
	if schemas.LearnUnsupportedParam(provider, "m", "prompt_cache_breakpoint") {
		t.Fatal("re-learning the same param should not report a change")
	}

	got := schemas.LearnedUnsupportedFor(provider, "m")
	if !got.StripCacheMarkers {
		t.Fatal("expected StripCacheMarkers after learning prompt_cache_breakpoint")
	}
	if !got.Params["temperature"] || len(got.Params) != 1 {
		t.Fatalf("expected only temperature in params, got %v", got.Params)
	}
	// A different model on the same provider learns nothing.
	if other := schemas.LearnedUnsupportedFor(provider, "other"); other.HasWork() {
		t.Fatal("learned params leaked across models")
	}
}

func markerChatRequest() *schemas.BifrostRequest {
	return &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-6.2-sol",
			Input: []schemas.ChatMessage{{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{
					Type:                  schemas.ChatContentBlockTypeText,
					Text:                  schemas.Ptr("hello"),
					CacheControl:          &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral},
					PromptCacheBreakpoint: &schemas.PromptCacheBreakpoint{Mode: schemas.Ptr("explicit")},
				}}},
			}},
			Params: &schemas.ChatParameters{
				Temperature: schemas.Ptr(0.5),
				Tools:       []schemas.ChatTool{{Type: schemas.ChatToolTypeFunction, CacheControl: &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral}}},
			},
		},
	}
}

func TestStripRequestCacheMarkers(t *testing.T) {
	req := markerChatRequest()
	original := req.ChatRequest.Input[0].Content.ContentBlocks[0]

	if !stripRequestCacheMarkers(req) {
		t.Fatal("expected stripRequestCacheMarkers to report a change")
	}
	block := req.ChatRequest.Input[0].Content.ContentBlocks[0]
	if block.CacheControl != nil || block.PromptCacheBreakpoint != nil {
		t.Fatal("content block markers not stripped")
	}
	if req.ChatRequest.Params.Tools[0].CacheControl != nil {
		t.Fatal("tool cache_control not stripped")
	}
	// The caller's shared structures keep their markers: stripping is
	// copy-on-write so a fallback to a marker-supporting provider still caches.
	if original.CacheControl == nil || original.PromptCacheBreakpoint == nil {
		t.Fatal("strip mutated the caller's original content block")
	}
	if stripRequestCacheMarkers(req) {
		t.Fatal("second strip should find no markers left")
	}
}

func TestStripRequestCacheMarkers_Responses(t *testing.T) {
	req := &schemas.BifrostRequest{
		RequestType: schemas.ResponsesRequest,
		ResponsesRequest: &schemas.BifrostResponsesRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-6.2-sol",
			Input: []schemas.ResponsesMessage{{
				CacheControl: &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral},
				Content: &schemas.ResponsesMessageContent{ContentBlocks: []schemas.ResponsesMessageContentBlock{{
					Type:         schemas.ResponsesInputMessageContentBlockTypeText,
					Text:         schemas.Ptr("hi"),
					CacheControl: &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral},
				}}},
			}},
			Params: &schemas.ResponsesParameters{
				Tools: []schemas.ResponsesTool{{
					Type:         schemas.ResponsesToolTypeFunction,
					CacheControl: &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral},
				}},
			},
		},
	}
	if !stripRequestCacheMarkers(req) {
		t.Fatal("expected stripRequestCacheMarkers to report a change")
	}
	msg := req.ResponsesRequest.Input[0]
	if msg.CacheControl != nil {
		t.Fatal("message-level cache_control not stripped")
	}
	if msg.Content.ContentBlocks[0].CacheControl != nil {
		t.Fatal("content block cache_control not stripped")
	}
	if req.ResponsesRequest.Params.Tools[0].CacheControl != nil {
		t.Fatal("tool cache_control not stripped")
	}
}

func TestDropLearnedParams(t *testing.T) {
	req := markerChatRequest()
	clone := cloneBifrostReq(req)
	learned := map[string]bool{"temperature": true, "prompt_cache_options": true}
	clone.ChatRequest.Params.PromptCacheOptions = &schemas.PromptCacheOptions{}

	dropped := dropLearnedParams(newTestContext(), clone, learned)
	if clone.ChatRequest.Params.Temperature != nil {
		t.Fatal("learned-unsupported temperature not dropped")
	}
	if clone.ChatRequest.Params.PromptCacheOptions != nil {
		t.Fatal("learned-unsupported prompt_cache_options not dropped")
	}
	if len(dropped) != 2 {
		t.Fatalf("expected 2 dropped params, got %v", dropped)
	}
	// Untouched params survive: the synthesized supported list must not drop
	// fields the learned set never named.
	if clone.ChatRequest.Params.Tools == nil {
		t.Fatal("tools dropped even though only temperature/prompt_cache_options were learned")
	}
}

// TestLearnedParamsEndToEnd runs the real plugin pair the way the orchestrator
// drives it: an upstream rejection is learned into the shared registry, and the
// next PreLLMHook — the same-pair retry's or a later request's — strips it.
func TestLearnedParamsEndToEnd(t *testing.T) {
	provider := uniqueLearnedProvider("e2e")
	model := "gpt-6.2-sol"
	p, err := Init(Config{LearnUnsupportedParams: true}, bifrost.NewNoOpLogger(), nil)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// What core's attempt loop does when the upstream rejects the param.
	bifrostErr := &schemas.BifrostError{
		Error: &schemas.ErrorField{Message: "prompt_cache_breakpoint is not supported on this model"},
	}
	bifrostErr.PopulateExtraFields(schemas.ChatCompletionRequest, provider, model, model)
	if param := schemas.UnsupportedParamFromError(bifrostErr); param == "" {
		t.Fatal("expected the rejection to name a param")
	} else if !schemas.LearnUnsupportedParam(provider, model, param) {
		t.Fatal("expected the learn to report a change")
	}

	req := markerChatRequest()
	req.ChatRequest.Provider = provider
	req.ChatRequest.Model = model
	out, _, err := p.PreLLMHook(newTestContext(), req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}
	if out == req {
		t.Fatal("expected PreLLMHook to return a clone once learned work exists")
	}
	if out.ChatRequest.Input[0].Content.ContentBlocks[0].CacheControl != nil {
		t.Fatal("learned prompt_cache_breakpoint did not strip cache markers")
	}
	// The original request keeps its markers for other providers' attempts.
	if req.ChatRequest.Input[0].Content.ContentBlocks[0].CacheControl == nil {
		t.Fatal("PreLLMHook mutated the caller's original request")
	}
}

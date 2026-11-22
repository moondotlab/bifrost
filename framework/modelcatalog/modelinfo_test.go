package modelcatalog

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/modelcatalog/datasheet"
	"github.com/maximhq/bifrost/framework/modelcatalog/keyconfig"
	"github.com/maximhq/bifrost/framework/modelcatalog/live"
	"github.com/stretchr/testify/require"
)

func modelInfoCatalog(t testing.TB) *ModelCatalog {
	t.Helper()
	pricingPath := filepath.Join(t.TempDir(), "pricing.json")
	pricingJSON := []byte(`{
		"claude-opus-5": {
			"provider": "anthropic",
			"mode": "chat",
			"base_model": "claude-opus-5",
			"max_input_tokens": 200000,
			"max_output_tokens": 64000,
			"input_cost_per_token": 0.000005,
			"output_cost_per_token": 0.000025,
			"cache_read_input_token_cost": 0.0000005,
			"cache_creation_input_token_cost": 0.00000625
		},
		"retired-model": {
			"provider": "anthropic",
			"mode": "chat",
			"base_model": "retired-model",
			"is_deprecated": true
		}
	}`)
	if err := os.WriteFile(pricingPath, pricingJSON, 0o600); err != nil {
		t.Fatalf("write pricing testdata: %v", err)
	}

	// Supported parameters live in a separate index fed by its own feed, so
	// seeding pricing alone leaves SupportedParameters empty and any assertion
	// about it passes for the wrong reason.
	paramsPath := filepath.Join(filepath.Dir(pricingPath), "params.json")
	paramsJSON := []byte(`{
		"claude-opus-5": {
			"mode": "chat",
			"model_parameters": [{"id": "temperature"}, {"id": "top_p"}]
		}
	}`)
	if err := os.WriteFile(paramsPath, paramsJSON, 0o600); err != nil {
		t.Fatalf("write model-parameters testdata: %v", err)
	}

	// Pricing resolution logs at debug level, so the store needs a real (no-op)
	// logger once a test prices a response rather than only reading metadata.
	ds := datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{
		URL:                "file://" + pricingPath,
		ModelParametersURL: "file://" + paramsPath,
	})
	if err := ds.LoadFromURLIntoMemory(t.Context()); err != nil {
		t.Fatalf("load pricing testdata: %v", err)
	}
	if err := ds.LoadModelParamsFromURLIntoMemory(t.Context()); err != nil {
		t.Fatalf("load model-parameters testdata: %v", err)
	}
	mc := &ModelCatalog{
		datasheet: ds,
		live:      live.New(nil),
		keyconf:   keyconfig.New(nil),
		done:      make(chan struct{}),
	}
	mc.initCaches()
	return mc
}

func TestGetModelInfoPopulatesPricingAndLimits(t *testing.T) {
	mc := modelInfoCatalog(t)

	info := mc.GetModelInfo(schemas.Anthropic, "claude-opus-5")
	if info == nil {
		t.Fatal("GetModelInfo = nil, want populated model")
	}
	if info.ID != "claude-opus-5" {
		t.Errorf("ID = %q, want claude-opus-5", info.ID)
	}
	// context_length is absent from the row, so it falls back to max_input_tokens.
	if info.ContextLength == nil || *info.ContextLength != 200000 {
		t.Errorf("ContextLength = %v, want 200000", info.ContextLength)
	}
	if info.MaxInputTokens == nil || *info.MaxInputTokens != 200000 {
		t.Errorf("MaxInputTokens = %v, want 200000", info.MaxInputTokens)
	}
	if info.MaxOutputTokens == nil || *info.MaxOutputTokens != 64000 {
		t.Errorf("MaxOutputTokens = %v, want 64000", info.MaxOutputTokens)
	}
	if info.Pricing == nil {
		t.Fatal("Pricing = nil, want populated")
	}
	// Fixed-decimal, never scientific notation: clients parse these as decimals.
	if info.Pricing.Prompt == nil || *info.Pricing.Prompt != "0.0000050000" {
		t.Errorf("Pricing.Prompt = %v, want 0.0000050000", info.Pricing.Prompt)
	}
	if info.Pricing.Completion == nil || *info.Pricing.Completion != "0.0000250000" {
		t.Errorf("Pricing.Completion = %v, want 0.0000250000", info.Pricing.Completion)
	}
	if info.Pricing.InputCacheRead == nil || *info.Pricing.InputCacheRead != "0.0000005000" {
		t.Errorf("Pricing.InputCacheRead = %v, want 0.0000005000", info.Pricing.InputCacheRead)
	}
	if info.Pricing.InputCacheWrite == nil || *info.Pricing.InputCacheWrite != "0.0000062500" {
		t.Errorf("Pricing.InputCacheWrite = %v, want 0.0000062500", info.Pricing.InputCacheWrite)
	}
}

func TestGetModelInfoReportsDeprecation(t *testing.T) {
	mc := modelInfoCatalog(t)

	info := mc.GetModelInfo(schemas.Anthropic, "retired-model")
	if info == nil {
		t.Fatal("GetModelInfo = nil, want populated model")
	}
	if !info.IsDeprecated {
		t.Error("IsDeprecated = false, want true")
	}
}

func TestGetModelInfoUnknownModel(t *testing.T) {
	mc := modelInfoCatalog(t)

	if got := mc.GetModelInfo(schemas.Anthropic, "not-a-real-model"); got != nil {
		t.Errorf("GetModelInfo for unknown model = %v, want nil", got)
	}
	if got := mc.GetModelInfo(schemas.Anthropic, ""); got != nil {
		t.Errorf("GetModelInfo for empty model = %v, want nil", got)
	}
}

// The catalog backfills what a provider's list-models response omitted; it must
// never overwrite what the provider actually reported.
func TestApplyModelInfoDoesNotOverwriteProviderValues(t *testing.T) {
	mc := modelInfoCatalog(t)
	entry := mc.datasheet.GetPricingEntryForModel("claude-opus-5", schemas.Anthropic)
	if entry == nil {
		t.Fatal("seeded pricing entry not found")
	}

	providerReported := 999
	model := &schemas.Model{
		ID:             "claude-opus-5",
		ContextLength:  &providerReported,
		MaxInputTokens: &providerReported,
		Pricing:        &schemas.Pricing{},
	}
	ApplyModelInfo(model, entry)

	if *model.ContextLength != providerReported {
		t.Errorf("ContextLength = %d, want provider-reported %d", *model.ContextLength, providerReported)
	}
	if *model.MaxInputTokens != providerReported {
		t.Errorf("MaxInputTokens = %d, want provider-reported %d", *model.MaxInputTokens, providerReported)
	}
	if model.Pricing.Prompt != nil {
		t.Errorf("Pricing.Prompt = %v, want untouched nil on a caller-set Pricing", model.Pricing.Prompt)
	}
	// Fields the provider left empty still get filled.
	if model.MaxOutputTokens == nil || *model.MaxOutputTokens != 64000 {
		t.Errorf("MaxOutputTokens = %v, want backfilled 64000", model.MaxOutputTokens)
	}
}

// GetModelInfo hands its result to third-party plugin code via ctx.GetModelInfo,
// and its doc promises the caller owns it. Entry fields alias the shared
// datasheet row, so every reference-typed field must be cloned on the way out:
// a write through an escaped handle would rewrite the catalog for all later
// requests, and racing the pricing sync on the attributes map is a fatal
// concurrent map access rather than a recoverable panic.
func TestGetModelInfoReturnsCallerOwnedCopy(t *testing.T) {
	mc := modelInfoCatalog(t)

	first := mc.GetModelInfo(schemas.Anthropic, "claude-opus-5")
	if first == nil {
		t.Fatal("GetModelInfo = nil, want populated model")
	}

	*first.ContextLength = 1
	*first.MaxInputTokens = 2
	*first.MaxOutputTokens = 3
	first.AdditionalAttributes = map[string]string{"tier": "mutated"}
	if len(first.SupportedParameters) == 0 {
		t.Fatal("SupportedParameters is empty, so mutating it would prove nothing")
	}
	first.SupportedParameters[0] = "mutated"

	second := mc.GetModelInfo(schemas.Anthropic, "claude-opus-5")
	if second == nil {
		t.Fatal("second GetModelInfo = nil, want populated model")
	}
	if *second.ContextLength != 200000 {
		t.Errorf("ContextLength = %d after caller mutation, want untouched 200000", *second.ContextLength)
	}
	if *second.MaxInputTokens != 200000 {
		t.Errorf("MaxInputTokens = %d after caller mutation, want untouched 200000", *second.MaxInputTokens)
	}
	if *second.MaxOutputTokens != 64000 {
		t.Errorf("MaxOutputTokens = %d after caller mutation, want untouched 64000", *second.MaxOutputTokens)
	}
	if second.AdditionalAttributes["tier"] == "mutated" {
		t.Error("AdditionalAttributes leaked a caller mutation into the catalog")
	}
	// SupportedParameters comes straight off the datasheet lookup rather than
	// through ApplyModelInfo's clones, so it needs its own check that the
	// lookup hands back a fresh slice.
	if slices.Contains(second.SupportedParameters, "mutated") {
		t.Errorf("SupportedParameters leaked a caller mutation into the catalog: %v", second.SupportedParameters)
	}

	// The pointers themselves must differ, else a later caller mutating through
	// one handle would still be seen by the other.
	if first.ContextLength == second.ContextLength {
		t.Error("ContextLength pointer is shared between calls, want a fresh allocation per call")
	}
	if first.MaxOutputTokens == second.MaxOutputTokens {
		t.Error("MaxOutputTokens pointer is shared between calls, want a fresh allocation per call")
	}
}

// The rows reached through ApplyModelInfo carry an Architecture and an
// attributes map, which GetModelInfo's seeded rows do not - cover those clones
// directly against the shared entry.
func TestApplyModelInfoClonesMutableEntryFields(t *testing.T) {
	entry := &PricingEntry{
		AdditionalAttributes: map[string]string{"tier": "original"},
		Architecture: &schemas.Architecture{
			// Architecture's scalar pointers are as shareable as its slices: a
			// struct copy carries them through untouched, and the entry's
			// Architecture is the pointer the datasheet itself stores.
			Modality:         new("text->text"),
			Tokenizer:        new("cl100k"),
			InstructType:     new("none"),
			InputModalities:  []string{"text"},
			OutputModalities: []string{"text"},
		},
	}

	model := &schemas.Model{ID: "claude-opus-5"}
	ApplyModelInfo(model, entry)

	if model.Architecture == entry.Architecture {
		t.Fatal("Architecture pointer is shared with the entry, want a clone")
	}
	if model.Architecture.Modality == entry.Architecture.Modality {
		t.Error("Architecture.Modality pointer is shared with the entry, want a clone")
	}
	if model.Architecture.Tokenizer == entry.Architecture.Tokenizer {
		t.Error("Architecture.Tokenizer pointer is shared with the entry, want a clone")
	}
	if model.Architecture.InstructType == entry.Architecture.InstructType {
		t.Error("Architecture.InstructType pointer is shared with the entry, want a clone")
	}

	model.AdditionalAttributes["tier"] = "mutated"
	model.Architecture.InputModalities[0] = "mutated"
	model.Architecture.OutputModalities[0] = "mutated"
	*model.Architecture.Modality = "mutated"
	*model.Architecture.Tokenizer = "mutated"
	*model.Architecture.InstructType = "mutated"

	if entry.AdditionalAttributes["tier"] != "original" {
		t.Errorf("entry AdditionalAttributes[tier] = %q, want untouched original", entry.AdditionalAttributes["tier"])
	}
	if entry.Architecture.InputModalities[0] != "text" {
		t.Errorf("entry InputModalities[0] = %q, want untouched text", entry.Architecture.InputModalities[0])
	}
	if entry.Architecture.OutputModalities[0] != "text" {
		t.Errorf("entry OutputModalities[0] = %q, want untouched text", entry.Architecture.OutputModalities[0])
	}
	if *entry.Architecture.Modality != "text->text" {
		t.Errorf("entry Modality = %q, want untouched text->text", *entry.Architecture.Modality)
	}
	if *entry.Architecture.Tokenizer != "cl100k" {
		t.Errorf("entry Tokenizer = %q, want untouched cl100k", *entry.Architecture.Tokenizer)
	}
	if *entry.Architecture.InstructType != "none" {
		t.Errorf("entry InstructType = %q, want untouched none", *entry.Architecture.InstructType)
	}
}

func TestApplyModelInfoNilSafe(t *testing.T) {
	ApplyModelInfo(nil, nil)
	ApplyModelInfo(&schemas.Model{}, nil)

	var nilCatalog *ModelCatalog
	if got := nilCatalog.GetModelInfo(schemas.Anthropic, "claude-opus-5"); got != nil {
		t.Errorf("GetModelInfo on nil catalog = %v, want nil", got)
	}
	if got := nilCatalog.CalculateRequestCost(nil, nil); got != 0 {
		t.Errorf("CalculateRequestCost on nil catalog = %v, want 0", got)
	}
	if got := nilCatalog.CalculateRequestCostBreakdown(nil, nil); got != nil {
		t.Errorf("CalculateRequestCostBreakdown on nil catalog = %v, want nil", got)
	}
	if got := modelInfoCatalog(t).CalculateRequestCostBreakdown(schemas.NewBifrostContext(nil, schemas.NoDeadline), nil); got != nil {
		t.Errorf("CalculateRequestCostBreakdown with nil response = %v, want nil", got)
	}
}

// Every models route enriches through these two methods, so they must resolve by
// ID, fall back to the alias, keep provider-reported values and be nil-safe.
func TestEnrichModelsResponsesFromCatalog(t *testing.T) {
	mc := modelInfoCatalog(t)
	providerLimit := 1000
	resp := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "anthropic/claude-opus-5"},
		{ID: "anthropic/my-deployment", Alias: schemas.Ptr("claude-opus-5")},
		{ID: "anthropic/claude-opus-5", MaxInputTokens: &providerLimit},
		{ID: "anthropic/retired-model"},
		{ID: "anthropic/unknown-model"},
	}}

	mc.EnrichListModelsResponse(resp)

	for i, want := range []int{200000, 200000, providerLimit} {
		got := resp.Data[i].MaxInputTokens
		if got == nil || *got != want {
			t.Errorf("Data[%d].MaxInputTokens = %v, want %d", i, got, want)
		}
		if out := resp.Data[i].MaxOutputTokens; out == nil || *out != 64000 {
			t.Errorf("Data[%d].MaxOutputTokens = %v, want 64000", i, out)
		}
	}
	if !resp.Data[3].IsDeprecated {
		t.Errorf("retired-model should be marked deprecated")
	}
	if resp.Data[4].MaxInputTokens != nil || resp.Data[4].Pricing != nil {
		t.Errorf("unknown model must stay unenriched, got %#v", resp.Data[4])
	}

	retrieved := &schemas.BifrostModelRetrieveResponse{Model: schemas.Model{ID: "anthropic/claude-opus-5"}}
	mc.EnrichModelRetrieveResponse(retrieved)
	if retrieved.MaxInputTokens == nil || *retrieved.MaxInputTokens != 200000 {
		t.Errorf("retrieve MaxInputTokens = %v, want 200000", retrieved.MaxInputTokens)
	}

	var nilCatalog *ModelCatalog
	nilCatalog.EnrichListModelsResponse(resp)
	nilCatalog.EnrichModelRetrieveResponse(retrieved)
	mc.EnrichListModelsResponse(nil)
	mc.EnrichModelRetrieveResponse(nil)
}

// cachedChatResponse is a chat completion whose prompt was partly served from
// the prompt cache and partly written to it, so every input-side category the
// seeded claude-opus-5 entry prices (text, cache read, cache write) is exercised.
func cachedChatResponse() *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Model: "claude-opus-5",
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens: 1000,
				PromptTokensDetails: &schemas.ChatPromptTokensDetails{
					CachedReadTokens:  200,
					CachedWriteTokens: 300,
				},
				CompletionTokens: 100,
				TotalTokens:      1100,
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType: schemas.ChatCompletionRequest,
				RoutingInfo: schemas.RoutingInfo{Provider: schemas.Anthropic, Model: "claude-opus-5"},
			},
		},
	}
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

// The plugin-facing breakdown is the same datasheet computation CalculateRequestCost
// reports as a scalar, so its TotalCost must match exactly and the input side must
// carry the cache read / cache write lines a plugin cannot recover from the total.
func TestCalculateRequestCostBreakdownMatchesTotal(t *testing.T) {
	mc := modelInfoCatalog(t)
	ctx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
	resp := cachedChatResponse()

	bd := mc.CalculateRequestCostBreakdown(ctx, resp)
	if bd == nil {
		t.Fatal("CalculateRequestCostBreakdown = nil, want a breakdown for billable usage")
	}
	if bd.InputCostDetails == nil || bd.OutputCostDetails == nil {
		t.Fatalf("details = input %v / output %v, want both populated", bd.InputCostDetails, bd.OutputCostDetails)
	}

	// Seeded rates: input 5e-6, output 25e-6, cache read 5e-7, cache write 6.25e-6.
	// 1000 prompt tokens = 500 uncached text + 200 cache read + 300 cache write.
	const (
		wantText      = 500 * 0.000005
		wantCacheRead = 200 * 0.0000005
		wantCacheWrit = 300 * 0.00000625
		wantOutput    = 100 * 0.000025
	)
	if !approxEqual(bd.InputCostDetails.TextCost, wantText) {
		t.Errorf("InputCostDetails.TextCost = %v, want %v", bd.InputCostDetails.TextCost, wantText)
	}
	if !approxEqual(bd.InputCostDetails.CachedReadCost, wantCacheRead) {
		t.Errorf("InputCostDetails.CachedReadCost = %v, want %v", bd.InputCostDetails.CachedReadCost, wantCacheRead)
	}
	if !approxEqual(bd.InputCostDetails.CachedWriteCost, wantCacheWrit) {
		t.Errorf("InputCostDetails.CachedWriteCost = %v, want %v", bd.InputCostDetails.CachedWriteCost, wantCacheWrit)
	}
	if !approxEqual(bd.OutputCostDetails.TextCost, wantOutput) {
		t.Errorf("OutputCostDetails.TextCost = %v, want %v", bd.OutputCostDetails.TextCost, wantOutput)
	}
	if !approxEqual(bd.InputCost, wantText+wantCacheRead+wantCacheWrit) {
		t.Errorf("InputCost = %v, want sum of its details %v", bd.InputCost, wantText+wantCacheRead+wantCacheWrit)
	}
	if !approxEqual(bd.InputCost+bd.OutputCost+bd.AdditionalCost, bd.TotalCost) {
		t.Errorf("input %v + output %v + additional %v != total %v", bd.InputCost, bd.OutputCost, bd.AdditionalCost, bd.TotalCost)
	}
	if total := mc.CalculateRequestCost(ctx, resp); total != bd.TotalCost {
		t.Errorf("CalculateRequestCost = %v, want breakdown TotalCost %v", total, bd.TotalCost)
	}
}

// The datasheet short-circuits to the provider-supplied usage.Cost pointer when a
// provider priced the request itself (Perplexity, xAI). Plugins own what the
// accessor returns, so that pointer must never leak through.
func TestCalculateRequestCostBreakdownReturnsCallerOwnedCopy(t *testing.T) {
	mc := modelInfoCatalog(t)
	ctx := schemas.NewBifrostContext(nil, schemas.NoDeadline)
	resp := cachedChatResponse()
	resp.ChatResponse.Usage.Cost = &schemas.BifrostCost{
		InputCost:        0.01,
		InputCostDetails: &schemas.InputCostDetails{TextCost: 0.008, CachedReadCost: 0.002},
		OutputCost:       0.02,
		TotalCost:        0.03,
	}

	bd := mc.CalculateRequestCostBreakdown(ctx, resp)
	if bd == nil {
		t.Fatal("CalculateRequestCostBreakdown = nil, want the provider-supplied cost")
	}
	if bd == resp.ChatResponse.Usage.Cost {
		t.Fatal("breakdown aliases resp.Usage.Cost, want a caller-owned copy")
	}
	if bd.InputCostDetails == resp.ChatResponse.Usage.Cost.InputCostDetails {
		t.Fatal("breakdown.InputCostDetails aliases the response's details, want a deep copy")
	}
	if bd.TotalCost != 0.03 || bd.InputCostDetails.CachedReadCost != 0.002 {
		t.Fatalf("copy = total %v / cache read %v, want 0.03 / 0.002", bd.TotalCost, bd.InputCostDetails.CachedReadCost)
	}

	bd.TotalCost = 99
	bd.InputCostDetails.CachedReadCost = 99
	if got := resp.ChatResponse.Usage.Cost; got.TotalCost != 0.03 || got.InputCostDetails.CachedReadCost != 0.002 {
		t.Fatalf("mutating the copy changed the response: total %v / cache read %v", got.TotalCost, got.InputCostDetails.CachedReadCost)
	}
}

// BenchmarkEnrichListModelsResponse measures the per-listing cost the drop-in
// models routes now pay. The "lookup-only" case is what they paid before: the
// same catalog lookup, keeping only the deprecation flag.
func BenchmarkEnrichListModelsResponse(b *testing.B) {
	mc := modelInfoCatalog(b)
	fresh := func() *schemas.BifrostListModelsResponse {
		data := make([]schemas.Model, 0, 50)
		for i := 0; i < 25; i++ {
			data = append(data, schemas.Model{ID: "anthropic/claude-opus-5"}, schemas.Model{ID: "anthropic/unknown-model"})
		}
		return &schemas.BifrostListModelsResponse{Data: data}
	}
	b.Run("lookup-only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			resp := fresh()
			b.StartTimer()
			for i := range resp.Data {
				provider, name := schemas.ParseModelString(resp.Data[i].ID, "")
				if e := mc.GetPricingEntryForModel(name, provider); e != nil && e.IsDeprecated {
					resp.Data[i].IsDeprecated = true
				}
			}
		}
	})
	b.Run("enrich", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			resp := fresh()
			b.StartTimer()
			mc.EnrichListModelsResponse(resp)
		}
	})
}

func TestModelCapabilityMetadata(t *testing.T) {
	params := filepath.Join(t.TempDir(), "params.json")
	ds := datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{ModelParametersURL: "file://" + params})
	ds.SetPricingRowsForTest([]tables.TableModelPricing{
		{Model: "multi", Provider: "openai", Mode: "embedding"},
		{Model: "multi", Provider: "openai", Mode: "image_generation"},
		{Model: "multi", Provider: "openai", Mode: "completion"},
		{Model: "multi", Provider: "openai", Mode: "responses"},
		{Model: "multi", Provider: "openai", Mode: "chat"},
		{Model: "multi", Provider: "anthropic", Mode: "decisions"},
		{Model: "unknown-mode", Provider: "openai", Mode: "z_custom"},
		{Model: "unknown-mode", Provider: "openai", Mode: "a_custom"},
	})
	require.NoError(t, os.WriteFile(params, []byte(`{
		"multi": {"provider":"anthropic", "supported_endpoints": ["/v1/ocr"]},
		"openai/params-only": {"mode":"live"},
		"openai/multi": {"mode":"rerank", "supported_endpoints": ["/v1/chat/completions", "/v1/images/variations", "/vendor/catalog"]}
	}`), 0o600))
	require.NoError(t, ds.LoadModelParamsFromURLIntoMemory(t.Context()))

	keys := keyconfig.New(nil)
	keys.Replace(map[schemas.ModelProvider][]schemas.Key{
		schemas.OpenAI: {{ID: "key", Enabled: schemas.Ptr(true), Models: schemas.WhiteList{"*"}, Aliases: schemas.KeyAliases{
			"named-alias": {ModelID: "wire-deployment", ModelName: schemas.Ptr("openai/multi")},
			"wire-alias":  {ModelID: "multi"},
		}}},
	})
	mc := &ModelCatalog{datasheet: ds, keyconf: keys}

	wantEndpoints := []string{
		"/v1/chat/completions", "/v1/responses", "/v1/completions",
		"/v1/embeddings", "/v1/images/generations", "/v1/rerank", "/v1/images/variations", "/vendor/catalog",
	}
	wantMethods := []string{
		string(schemas.ChatCompletionRequest), string(schemas.ResponsesRequest), string(schemas.TextCompletionRequest),
		string(schemas.EmbeddingRequest), string(schemas.ImageGenerationRequest), string(schemas.RerankRequest), string(schemas.ImageVariationRequest),
	}
	for _, model := range []schemas.Model{
		{ID: "openai/multi"},
		{ID: "openai/list-alias", Alias: schemas.Ptr("openai/multi")},
		{ID: "openai/named-alias"},
		{ID: "openai/wire-alias"},
		{ID: "openai/multi-2024-01-01"},
	} {
		t.Run(model.ID, func(t *testing.T) {
			list := &schemas.BifrostListModelsResponse{Data: []schemas.Model{model}}
			retrieve := &schemas.BifrostModelRetrieveResponse{Model: model}
			mc.EnrichListModelsResponse(list)
			mc.EnrichModelRetrieveResponse(retrieve)
			got := list.Data[0]
			require.Equal(t, got, retrieve.Model)
			require.Equal(t, "chat", got.Mode)
			require.Equal(t, wantEndpoints, got.SupportedEndpoints)
			require.Equal(t, wantMethods, got.SupportedMethods)
			require.Nil(t, got.Pricing)
			if model.Alias == nil {
				_, name := schemas.ParseModelString(model.ID, "")
				info := mc.GetModelInfo(schemas.OpenAI, name)
				require.NotNil(t, info)
				require.Equal(t, got.Mode, info.Mode)
				require.Equal(t, got.SupportedEndpoints, info.SupportedEndpoints)
				require.Equal(t, got.SupportedMethods, info.SupportedMethods)
			}
		})
	}

	list := &schemas.BifrostListModelsResponse{Data: []schemas.Model{
		{ID: "anthropic/multi"},
		{ID: "openai/unknown-mode"},
		{ID: "openai/no-data"},
		{ID: "openai/multi", Mode: "provider-mode",
			SupportedEndpoints: []string{"/provider/endpoint", "/v1/chat/completions"},
			SupportedMethods:   []string{"provider-method", string(schemas.ChatCompletionRequest)}},
	}}
	mc.EnrichListModelsResponse(list)
	require.Equal(t, "decisions", list.Data[0].Mode)
	require.Equal(t, []string{"/v1/decisions", "/v1/ocr"}, list.Data[0].SupportedEndpoints)
	require.Equal(t, []string{string(schemas.DecisionRequest), string(schemas.OCRRequest)}, list.Data[0].SupportedMethods)
	require.Equal(t, "a_custom", list.Data[1].Mode)
	unknownJSON, err := json.Marshal(list.Data[1])
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"openai/unknown-mode","mode":"a_custom"}`, string(unknownJSON))
	emptyJSON, err := json.Marshal(list.Data[2])
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"openai/no-data"}`, string(emptyJSON))
	require.Equal(t, "provider-mode", list.Data[3].Mode)
	require.Equal(t, append([]string{"/provider/endpoint"}, wantEndpoints...), list.Data[3].SupportedEndpoints)
	require.Equal(t, append([]string{"provider-method"}, wantMethods...), list.Data[3].SupportedMethods)

	info := mc.GetModelInfo(schemas.OpenAI, "params-only")
	require.NotNil(t, info)
	require.Equal(t, "live", info.Mode)
	require.Equal(t, []string{"/v1/live/sessions"}, info.SupportedEndpoints)
	require.Equal(t, []string{string(schemas.LiveRequest)}, info.SupportedMethods)
	require.Nil(t, mc.GetModelInfo(schemas.Anthropic, "params-only"))
}

func TestModelCapabilityMappingCoversPricingModes(t *testing.T) {
	ds := datasheet.NewTestStore(nil)
	mc := &ModelCatalog{datasheet: ds}
	wantEndpoints := map[string]string{
		"chat": "/v1/chat/completions", "responses": "/v1/responses", "completion": "/v1/completions",
		"embedding": "/v1/embeddings", "rerank": "/v1/rerank", "decisions": "/v1/decisions",
		"audio_speech": "/v1/audio/speech", "audio_transcription": "/v1/audio/transcriptions",
		"image_generation": "/v1/images/generations", "image_edit": "/v1/images/edits",
		"video_generation": "/v1/videos", "ocr": "/v1/ocr",
		"container_create": "/v1/containers", "live": "/v1/live/sessions",
	}
	for _, mode := range datasheet.PricingLookupModes() {
		t.Run(mode, func(t *testing.T) {
			ds.SetPricingRowsForTest([]tables.TableModelPricing{{Model: "test", Provider: "openai", Mode: mode}})
			info := mc.GetModelInfo(schemas.OpenAI, "test")
			require.NotNil(t, info)
			require.Equal(t, mode, info.Mode)
			require.Equal(t, []string{wantEndpoints[mode]}, info.SupportedEndpoints)
			require.Len(t, info.SupportedMethods, 1)
			require.NotNil(t, ds.Get("test", schemas.OpenAI, schemas.RequestType(info.SupportedMethods[0])))
		})
	}
}

func TestCustomProviderModelCapabilityConsensus(t *testing.T) {
	const (
		provider = schemas.ModelProvider("modelflare")
		name     = "deepseek-v4-flash"
		id       = "modelflare/" + name
		chat     = "/v1/chat/completions"
	)
	schemas.RegisterKnownProvider(provider)
	t.Cleanup(func() { schemas.UnregisterKnownProvider(provider) })

	params := filepath.Join(t.TempDir(), "params.json")
	ds := datasheet.New(nil, bifrost.NewNoOpLogger(), datasheet.Config{ModelParametersURL: "file://" + params})
	loadParams := func(data string) {
		t.Helper()
		require.NoError(t, os.WriteFile(params, []byte(data), 0o600))
		require.NoError(t, ds.LoadModelParamsFromURLIntoMemory(t.Context()))
	}
	rows := []tables.TableModelPricing{
		{Model: "deepseek/" + name, Provider: "deepseek", Mode: "chat", InputCostPerToken: new(0.001),
			MaxInputTokens: new(1000), MaxOutputTokens: new(2000), ContextLength: new(3000), IsDeprecated: true},
		{Model: name, Provider: "azure", Mode: "chat"},
		{Model: name, Provider: "azure", Mode: "responses"},
		{Model: "openrouter/deepseek/" + name, Provider: "openrouter", Mode: "embedding"},
		{Model: "fireworks_ai/" + name, Provider: "fireworks_ai", Mode: "chat"},
	}
	ds.SetPricingRowsForTest(rows)
	loadParams(`{
		"azure/deepseek-v4-flash": {"supported_endpoints":["/v1/chat/completions","/v1/responses","/v1/messages"]},
		"deepseek/other-model": {"mode":"chat"},
		"deepseek-v4-flash": {"provider":"deepseek","supports_function_calling":true}
	}`)

	keys := keyconfig.New(nil)
	keys.Replace(map[schemas.ModelProvider][]schemas.Key{
		provider: {{ID: "key", Enabled: new(true), Models: schemas.WhiteList{"*"}, Aliases: schemas.KeyAliases{
			"canonical":      {ModelID: "deployment", ModelName: new("deepseek/" + name)},
			"wire":           {ModelID: name},
			"qualified-wire": {ModelID: "deepseek/" + name},
			"exact-wire":     {ModelID: "own-wire", ModelName: new("other-model")},
			"exact-name":     {ModelID: name, ModelName: new("own-name")},
		}}},
	})
	mc := &ModelCatalog{datasheet: ds, keyconf: keys}
	enrich := func(model schemas.Model) schemas.Model {
		t.Helper()
		list := &schemas.BifrostListModelsResponse{Data: []schemas.Model{model}}
		retrieve := &schemas.BifrostModelRetrieveResponse{Model: model}
		mc.EnrichListModelsResponse(list)
		mc.EnrichModelRetrieveResponse(retrieve)
		require.Equal(t, list.Data[0], retrieve.Model)
		return retrieve.Model
	}
	checkNoPricing := func(model schemas.Model) {
		t.Helper()
		require.Nil(t, model.Pricing)
		require.Nil(t, model.MaxInputTokens)
		require.Nil(t, model.MaxOutputTokens)
		require.Nil(t, model.ContextLength)
		require.False(t, model.IsDeprecated)
	}
	for _, model := range []schemas.Model{
		{ID: id},
		{ID: "modelflare/list-alias", Alias: new("deepseek/" + name)},
		{ID: "modelflare/canonical"},
		{ID: "modelflare/wire"},
		{ID: "modelflare/qualified-wire"},
	} {
		got := enrich(model)
		require.Equal(t, "chat", got.Mode, model.ID)
		require.Equal(t, []string{chat}, got.SupportedEndpoints, model.ID)
		require.Equal(t, []string{string(schemas.ChatCompletionRequest)}, got.SupportedMethods, model.ID)
		checkNoPricing(got)
	}
	info := mc.GetModelInfo(provider, name)
	require.NotNil(t, info)
	require.Equal(t, "chat", info.Mode)
	require.Equal(t, []string{chat}, info.SupportedEndpoints)
	checkNoPricing(*info)
	require.Contains(t, ds.GetSupportedParameters(name), "tools")
	require.Empty(t, info.SupportedParameters)
	require.Nil(t, mc.GetPricingEntryForModel(name, provider))
	require.Nil(t, ds.GetCapabilityEntry(name, provider))

	got := enrich(schemas.Model{ID: id, Mode: "reported", SupportedEndpoints: []string{"/reported"}, SupportedMethods: []string{"reported"}})
	require.Equal(t, "reported", got.Mode)
	require.Equal(t, []string{"/reported", chat}, got.SupportedEndpoints)
	require.Equal(t, []string{"reported", string(schemas.ChatCompletionRequest)}, got.SupportedMethods)

	loadParams(`{
		"azure/deepseek-v4-flash": {"supported_endpoints":["/v1/chat/completions","/v1/ocr","/v1/responses","/v1/messages"]},
		"deepseek/deepseek-v4-flash": {"supported_endpoints":["/v1/chat/completions","/v1/ocr","/vendor/deepseek"]}
	}`)
	got = enrich(schemas.Model{ID: id})
	require.Equal(t, "chat", got.Mode)
	require.Equal(t, []string{chat, "/v1/ocr"}, got.SupportedEndpoints)
	require.Equal(t, []string{string(schemas.ChatCompletionRequest), string(schemas.OCRRequest)}, got.SupportedMethods)

	loadParams(`{
		"azure/deepseek-v4-flash": {"supported_endpoints":["/v1/responses"]},
		"deepseek/deepseek-v4-flash": {"supported_endpoints":["/v1/messages"]},
		"modelflare/own-name": {"mode":"rerank","supported_endpoints":["/vendor/modelflare"]}
	}`)
	require.Equal(t, []string{chat}, enrich(schemas.Model{ID: id}).SupportedEndpoints)
	ds.SetPricingRowsForTest(append(slices.Clone(rows),
		tables.TableModelPricing{Model: name, Provider: string(provider), Mode: "completion", InputCostPerToken: new(0.05)},
		tables.TableModelPricing{Model: "own-wire", Provider: string(provider), Mode: "embedding"},
	))
	got = enrich(schemas.Model{ID: id})
	require.Equal(t, "completion", got.Mode)
	require.Equal(t, []string{"/v1/completions"}, got.SupportedEndpoints)
	require.NotNil(t, got.Pricing)
	require.Equal(t, "0.0500000000", *got.Pricing.Prompt)
	got = enrich(schemas.Model{ID: "modelflare/exact-wire"})
	require.Equal(t, "embedding", got.Mode)
	require.Equal(t, []string{"/v1/embeddings"}, got.SupportedEndpoints)
	got = enrich(schemas.Model{ID: "modelflare/exact-name"})
	require.Equal(t, "rerank", got.Mode)
	require.Equal(t, []string{"/v1/rerank", "/vendor/modelflare"}, got.SupportedEndpoints)

	loadParams(`{"deepseek/other-model":{"mode":"chat"}}`)
	ds.SetPricingRowsForTest([]tables.TableModelPricing{rows[0], rows[3],
		{Model: name, Provider: "azure", Mode: "embedding"},
	})
	got = enrich(schemas.Model{ID: id})
	require.Empty(t, got.Mode)
	require.Empty(t, got.SupportedEndpoints)
	require.Nil(t, mc.GetModelInfo(provider, name))

	ds.SetPricingRowsForTest([]tables.TableModelPricing{rows[3],
		{Model: name + "-2024-01-01", BaseModel: name, Provider: "deepseek", Mode: "chat"},
	})
	got = enrich(schemas.Model{ID: id})
	require.Empty(t, got.Mode)
	require.Empty(t, got.SupportedEndpoints)
	require.Nil(t, mc.GetModelInfo(provider, name))
}

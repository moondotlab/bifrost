// Package providerhealth is a fork-private Bifrost plugin that watches per-attempt
// outcomes and temporarily degrades providers whose accounts are broken: consecutive
// credential or quota failures (key rejected, insufficient credit) exclude the provider
// from routing while healthy alternatives exist. Degradation is time-based with
// exponential backoff, so recovery needs no probing traffic — the provider simply
// rejoins when its cooldown lapses and either serves (resetting its state) or fails
// again (re-opening with doubled backoff).
//
// Routing integration lives in the plugins that pick providers:
//   - modelcatalogresolver drops degraded candidates unless every candidate is degraded
//   - governance's weighted load balancing does the same for its eligible pool
//
// Both read the package-level Default tracker this plugin records into.
package providerhealth

import (
	"context"
	"time"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

// PluginName is the name of the provider health plugin.
const PluginName = "providerhealth"

// Plugin records per-attempt provider outcomes into a Tracker.
type Plugin struct {
	tracker *Tracker
	logger  schemas.Logger
}

// ConfigJSON is the optional plugin configuration. All fields optional.
type ConfigJSON struct {
	FailureThreshold    int `json:"failure_threshold"`
	BaseCooldownSeconds int `json:"base_cooldown_seconds"`
	MaxCooldownSeconds  int `json:"max_cooldown_seconds"`
}

// Init creates the plugin. A nil config uses production defaults; otherwise the config
// may be a map[string]any (from plugin config JSON) with failure_threshold,
// base_cooldown_seconds and max_cooldown_seconds. The plugin records into the shared
// Default tracker so the routing plugins observe the same state.
func Init(_ context.Context, config any, logger schemas.Logger) (*Plugin, error) {
	cfg := DefaultConfig()
	if m, ok := config.(map[string]any); ok {
		if v, ok := m["failure_threshold"].(float64); ok && v > 0 {
			cfg.FailureThreshold = int(v)
		}
		if v, ok := m["base_cooldown_seconds"].(float64); ok && v > 0 {
			cfg.BaseCooldown = time.Duration(v) * time.Second
		}
		if v, ok := m["max_cooldown_seconds"].(float64); ok && v > 0 {
			cfg.MaxCooldown = time.Duration(v) * time.Second
		}
	}
	// Rebuild the shared tracker so config applies process-wide. State is in-memory and
	// cheap to relearn, so discarding pre-init state (normally none) is fine.
	Default = NewTracker(cfg)
	return &Plugin{tracker: Default, logger: logger}, nil
}

// GetName implements schemas.BasePlugin.
func (p *Plugin) GetName() string {
	return PluginName
}

// PreRequestHook implements schemas.LLMPlugin (no-op: health is observed, not enforced here).
func (p *Plugin) PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	return nil
}

// PreLLMHook implements schemas.LLMPlugin (no-op: health is observed, not enforced here).
func (p *Plugin) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}

// PostLLMHook implements schemas.LLMPlugin. It runs once per provider attempt (and per
// stream chunk) and is the plugin's only job: classify the outcome and record it.
func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, result *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	requestType, provider, _, _ := bifrost.GetResponseFields(result, bifrostErr)
	if provider == "" {
		return result, bifrostErr, nil
	}
	if bifrostErr != nil {
		class := bifrost.ClassifyFailure(bifrostErr)
		if CountedFailure(class) && p.tracker.RecordFailure(provider, class) {
			p.logger.Warn("providerhealth: provider %s degraded after consecutive %s failures; temporarily excluded from routing while healthy alternatives exist", provider, class)
		}
		return result, bifrostErr, nil
	}
	if result == nil {
		return result, bifrostErr, nil
	}
	// Success. Streaming responses report per chunk; only the final chunk settles the
	// attempt, otherwise a stream that dies halfway would already have reset the streak.
	if bifrost.IsStreamRequestType(requestType) && !bifrost.IsFinalChunk(ctx) {
		return result, bifrostErr, nil
	}
	p.tracker.RecordSuccess(provider)
	return result, bifrostErr, nil
}

// Cleanup implements schemas.BasePlugin.
func (p *Plugin) Cleanup() error {
	return nil
}

var _ schemas.LLMPlugin = (*Plugin)(nil)

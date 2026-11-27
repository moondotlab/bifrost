package schemas

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// This file implements learned parameter compatibility, the detection and
// bookkeeping half of the compat plugin's unsupported-param handling. When an
// upstream rejects a request field the model catalog believes in
// ("prompt_cache_breakpoint is not supported on this model"), the
// (provider, model) pair is remembered here so that later attempts — including
// a same-request retry the orchestrator may run — go out without the field
// instead of paying a failed round trip every time. Reseller providers serving
// OpenAI/Azure/Anthropic model names routinely lag the datasheet's capability
// claims. The compat plugin owns the request rewriting; core owns the retry
// decision; both read and write this one registry.

// LearnedEntryTTL bounds how long a learned rejection is honored. Upstreams can
// gain support for a field; a stale entry costs the caller the field's benefit
// (e.g. explicit cache pricing) but never correctness, while a too-short TTL
// costs one failed request per window.
const LearnedEntryTTL = 24 * time.Hour

// learnedEntry is the per-(provider, model) record of proven rejections.
type learnedEntry struct {
	mu                sync.Mutex
	params            map[string]bool // neutral param names to drop from Params
	stripCacheMarkers bool            // remove cache_control / prompt_cache_breakpoint message markers
	learnedAt         time.Time
}

// learnedUnsupported maps "provider\x1fmodel" to *learnedEntry. Package-level
// because the fact belongs to the upstream, not to any one request or plugin
// instance.
var learnedUnsupported sync.Map

// LearnedUnsupportedSet is a consistent snapshot of one entry.
type LearnedUnsupportedSet struct {
	Params            map[string]bool
	StripCacheMarkers bool
}

// HasWork reports whether the set would change a request.
func (s LearnedUnsupportedSet) HasWork() bool {
	return s.StripCacheMarkers || len(s.Params) > 0
}

func learnedKey(provider ModelProvider, model string) string {
	return string(provider) + "\x1f" + model
}

// LearnUnsupportedParam records that provider/model rejected param, returning
// true when the learned set actually changed (a new param or a newly learned
// marker strip). Callers use the change signal to decide whether an immediate
// retry can differ from the attempt that just failed. Unknown or unstrippable
// names are ignored: learning a param nobody can remove would only log noise.
func LearnUnsupportedParam(provider ModelProvider, model, param string) bool {
	param = normalizeLearnedParam(param)
	if param == "" || !learnableParams[param] {
		return false
	}
	key := learnedKey(provider, model)
	v, _ := learnedUnsupported.LoadOrStore(key, &learnedEntry{params: make(map[string]bool)})
	e := v.(*learnedEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.learnedAt = time.Now()
	if param == "prompt_cache_breakpoint" || param == "cache_control" {
		if e.stripCacheMarkers {
			return false
		}
		e.stripCacheMarkers = true
		return true
	}
	if e.params[param] {
		return false
	}
	e.params[param] = true
	return true
}

// LearnedUnsupportedFor returns the learned rejections for a (provider, model)
// pair, or the zero set when none are known or the entry expired.
func LearnedUnsupportedFor(provider ModelProvider, model string) LearnedUnsupportedSet {
	if provider == "" || model == "" {
		return LearnedUnsupportedSet{}
	}
	v, ok := learnedUnsupported.Load(learnedKey(provider, model))
	if !ok {
		return LearnedUnsupportedSet{}
	}
	e := v.(*learnedEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.learnedAt) > LearnedEntryTTL {
		learnedUnsupported.Delete(learnedKey(provider, model))
		return LearnedUnsupportedSet{}
	}
	set := LearnedUnsupportedSet{StripCacheMarkers: e.stripCacheMarkers}
	if len(e.params) > 0 {
		set.Params = make(map[string]bool, len(e.params))
		for p := range e.params {
			set.Params[p] = true
		}
	}
	return set
}

// normalizeLearnedParam reduces an upstream-reported field name to the neutral
// param spelling used internally: surrounding punctuation and path prefixes
// ("input[3].content[0].prompt_cache_breakpoint") collapse to the leaf name, and
// provider spellings map onto the neutral ones ("reasoning_effort" is the chat
// wire's name for "reasoning").
func normalizeLearnedParam(param string) string {
	param = strings.TrimSpace(param)
	if i := strings.LastIndexFunc(param, func(r rune) bool {
		return r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}); i >= 0 {
		param = param[i+1:]
	}
	switch strings.ToLower(param) {
	case "reasoning_effort", "reasoning_effort_beta":
		return "reasoning"
	}
	return param
}

// learnableParams is the whitelist of fields a compat layer can remove:
// request Params members plus the message-level cache markers. Anything else an
// upstream names ("model", "messages", a provider-specific extension) is left
// alone rather than guessed at.
var learnableParams = map[string]bool{
	"audio": true, "frequency_penalty": true, "logit_bias": true, "logprobs": true,
	"max_completion_tokens": true, "max_output_tokens": true, "max_tokens": true,
	"max_tool_calls": true, "metadata": true, "n": true, "parallel_tool_calls": true,
	"prediction": true, "presence_penalty": true, "prompt_cache_breakpoint": true,
	"prompt_cache_key": true, "prompt_cache_options": true, "prompt_cache_retention": true,
	"reasoning": true, "response_format": true, "seed": true, "service_tier": true,
	"stop": true, "temperature": true, "text": true, "tool_choice": true,
	"tools": true, "top_logprobs": true, "top_p": true, "verbosity": true,
	"web_search_options": true, "cache_control": true,
}

// unsupportedParamPatterns match the error phrasing providers use for a
// rejected field. The whitelist above does the real filtering; these only need
// to surface a candidate name.
var unsupportedParamPatterns = []*regexp.Regexp{
	// "prompt_cache_breakpoint is not supported on this model"
	regexp.MustCompile(`(?i)\b([a-zA-Z_][a-zA-Z0-9_]*)\s+is not supported\b`),
	// "Unsupported parameter: 'temperature'"
	regexp.MustCompile(`(?i)\bunsupported\s+(?:request\s+)?(?:parameter|param|field|argument)\b[^a-zA-Z0-9_]{0,4}["'` + "`" + `]?([a-zA-Z_][a-zA-Z0-9_.]*)`),
	// "Unrecognized request argument supplied: foo"
	regexp.MustCompile(`(?i)\bunrecognized\s+(?:request\s+)?argument(?:\s+supplied)?\b[^a-zA-Z0-9_]{0,6}["'` + "`" + `]?([a-zA-Z_][a-zA-Z0-9_.]*)`),
	// "Unknown parameter: foo"
	regexp.MustCompile(`(?i)\bunknown\s+(?:parameter|param|field|argument|property)\b[^a-zA-Z0-9_]{0,4}["'` + "`" + `]?([a-zA-Z_][a-zA-Z0-9_.]*)`),
}

// UnsupportedParamFromError extracts the rejected field name from an upstream
// error, or "" when the error is not a parameter rejection. Only errors that
// came from the provider are considered: a Bifrost-internal error never saw an
// upstream and cannot teach anything.
func UnsupportedParamFromError(bifrostErr *BifrostError) string {
	if bifrostErr == nil || bifrostErr.IsBifrostError || bifrostErr.Error == nil {
		return ""
	}
	if p, ok := bifrostErr.Error.Param.(string); ok {
		if name := normalizeLearnedParam(p); learnableParams[name] {
			return name
		}
	}
	for _, re := range unsupportedParamPatterns {
		if m := re.FindStringSubmatch(bifrostErr.Error.Message); m != nil {
			if name := normalizeLearnedParam(m[1]); learnableParams[name] {
				return name
			}
		}
	}
	return ""
}

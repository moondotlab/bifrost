package compat

import (
	"slices"

	"github.com/maximhq/bifrost/core/schemas"
)

// This file is the request-rewriting half of learned parameter compatibility.
// Detection, the (provider, model) registry and the retry signal live in
// core/schemas (learnedparams.go): the orchestrator learns from upstream
// rejections and retries the same pair once, and this plugin makes every later
// attempt — the retry included — go out without the rejected field.

// allDroppableParamNames is every capability key dropUnsupportedParams reads,
// plus prompt_cache_options. Passing it minus the learned set as the "supported"
// list makes the learned-only path drop exactly the proven-rejected params while
// leaving everything else alone.
var allDroppableParamNames = []string{
	"assistant_prefill", "audio", "frequency_penalty", "logit_bias", "logprobs",
	"max_completion_tokens", "max_output_tokens", "max_tokens", "max_tool_calls",
	"metadata", "n", "parallel_tool_calls", "prediction", "presence_penalty",
	"prompt_cache_key", "prompt_cache_options", "prompt_cache_retention",
	"reasoning", "reasoning_summary_auto", "reasoning_summary_concise",
	"reasoning_summary_detailed", "reasoning_with_tool_calls", "response_format",
	"seed", "service_tier", "stop", "supports_none_reasoning_effort",
	"temperature", "text", "tool_choice", "tools", "top_logprobs", "top_p",
	"verbosity", "web_search", "web_search_options",
}

// dropLearnedParams removes learned-rejected params from req. req must already
// be a clone: the params structs are shared with the caller's request otherwise.
func dropLearnedParams(ctx *schemas.BifrostContext, req *schemas.BifrostRequest, learned map[string]bool) []string {
	supported := make([]string, 0, len(allDroppableParamNames))
	for _, name := range allDroppableParamNames {
		if !learned[name] {
			supported = append(supported, name)
		}
	}
	// dropUnsupportedParams also publishes the assistant_prefill capability,
	// which the synthesized "everything supported" list would reset to true
	// after the catalog pass may have set it false. Restore whatever the
	// catalog decided; the learned set never names assistant_prefill.
	prefill, hadPrefill := ctx.Value(schemas.BifrostContextKeySupportsAssistantPrefill).(bool)
	dropped := dropUnsupportedParams(ctx, req, supported)
	if hadPrefill {
		ctx.SetValue(schemas.BifrostContextKeySupportsAssistantPrefill, prefill)
	}
	return dropped
}

// stripRequestCacheMarkers removes every prompt-cache marker — cache_control
// and prompt_cache_breakpoint, on messages, content blocks, tool definitions,
// assistant tool calls and tool-call outputs — because an upstream that rejects
// prompt_cache_breakpoint rejects the translated form of all of them. Each
// container is copied before it is changed, so the caller's shared request
// keeps its markers for attempts that go to providers which do support them.
// req must already be a clone (cloneBifrostReq); the Input slices it shares with
// the original are cloned here before their elements are touched.
func stripRequestCacheMarkers(req *schemas.BifrostRequest) bool {
	stripped := false
	if chat := req.ChatRequest; chat != nil {
		chatReq := *chat
		chatReq.Input = slices.Clone(chat.Input)
		for i := range chatReq.Input {
			stripped = stripChatMessageCacheMarkers(&chatReq.Input[i]) || stripped
		}
		if chat.Params != nil && len(chat.Params.Tools) > 0 {
			params := *chat.Params
			params.Tools = slices.Clone(chat.Params.Tools)
			for i := range params.Tools {
				if params.Tools[i].CacheControl != nil {
					params.Tools[i].CacheControl = nil
					stripped = true
				}
			}
			chatReq.Params = &params
		}
		req.ChatRequest = &chatReq
	}
	if resp := req.ResponsesRequest; resp != nil {
		respReq := *resp
		respReq.Input = slices.Clone(resp.Input)
		for i := range respReq.Input {
			stripped = stripResponsesMessageCacheMarkers(&respReq.Input[i]) || stripped
		}
		if resp.Params != nil && len(resp.Params.Tools) > 0 {
			params := *resp.Params
			params.Tools = slices.Clone(resp.Params.Tools)
			for i := range params.Tools {
				if params.Tools[i].CacheControl != nil {
					params.Tools[i].CacheControl = nil
					stripped = true
				}
			}
			respReq.Params = &params
		}
		req.ResponsesRequest = &respReq
	}
	return stripped
}

func stripChatMessageCacheMarkers(msg *schemas.ChatMessage) bool {
	stripped := false
	if msg.Content != nil && len(msg.Content.ContentBlocks) > 0 {
		blocks := slices.Clone(msg.Content.ContentBlocks)
		blockStripped := false
		for i := range blocks {
			if blocks[i].CacheControl != nil || blocks[i].PromptCacheBreakpoint != nil {
				blocks[i].CacheControl = nil
				blocks[i].PromptCacheBreakpoint = nil
				blockStripped = true
			}
		}
		if blockStripped {
			content := *msg.Content
			content.ContentBlocks = blocks
			msg.Content = &content
			stripped = true
		}
	}
	if msg.ChatAssistantMessage != nil && len(msg.ChatAssistantMessage.ToolCalls) > 0 {
		assistant := *msg.ChatAssistantMessage
		toolCalls := slices.Clone(assistant.ToolCalls)
		callsStripped := false
		for i := range toolCalls {
			if toolCalls[i].CacheControl != nil {
				toolCalls[i].CacheControl = nil
				callsStripped = true
			}
		}
		if callsStripped {
			assistant.ToolCalls = toolCalls
			msg.ChatAssistantMessage = &assistant
			stripped = true
		}
	}
	return stripped
}

func stripResponsesContentBlockMarkers(blocks []schemas.ResponsesMessageContentBlock) bool {
	stripped := false
	for i := range blocks {
		if blocks[i].CacheControl != nil || blocks[i].PromptCacheBreakpoint != nil {
			blocks[i].CacheControl = nil
			blocks[i].PromptCacheBreakpoint = nil
			stripped = true
		}
	}
	return stripped
}

func stripResponsesMessageCacheMarkers(msg *schemas.ResponsesMessage) bool {
	stripped := false
	if msg.CacheControl != nil {
		msg.CacheControl = nil
		stripped = true
	}
	if msg.Content != nil && len(msg.Content.ContentBlocks) > 0 {
		blocks := slices.Clone(msg.Content.ContentBlocks)
		if stripResponsesContentBlockMarkers(blocks) {
			content := *msg.Content
			content.ContentBlocks = blocks
			msg.Content = &content
			stripped = true
		}
	}
	if msg.ResponsesToolMessage != nil && msg.ResponsesToolMessage.Output != nil &&
		len(msg.ResponsesToolMessage.Output.ResponsesFunctionToolCallOutputBlocks) > 0 {
		toolMsg := *msg.ResponsesToolMessage
		output := *toolMsg.Output
		blocks := slices.Clone(output.ResponsesFunctionToolCallOutputBlocks)
		if stripResponsesContentBlockMarkers(blocks) {
			output.ResponsesFunctionToolCallOutputBlocks = blocks
			toolMsg.Output = &output
			msg.ResponsesToolMessage = &toolMsg
			stripped = true
		}
	}
	return stripped
}

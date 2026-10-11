package openai

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// usageFromClaude reads an Anthropic usage object, cache fields included.
func usageFromClaude(u gjson.Result) usage.Detail {
	d := usage.Detail{
		InputTokens:         u.Get("input_tokens").Int(),
		OutputTokens:        u.Get("output_tokens").Int(),
		CacheReadTokens:     u.Get("cache_read_input_tokens").Int(),
		CacheCreationTokens: u.Get("cache_creation_input_tokens").Int(),
	}
	d.CachedTokens = d.CacheReadTokens
	d.TotalTokens = d.InputTokens + d.CacheReadTokens + d.CacheCreationTokens + d.OutputTokens
	return d
}

// openAIUsage renders usage in the Chat Completions shape. OpenAI counts the
// whole prompt in prompt_tokens and reports the cached part separately, so
// Anthropic's cache read and cache write are folded back into the prompt.
func openAIUsage(d usage.Detail) map[string]interface{} {
	prompt := d.InputTokens + d.CacheReadTokens + d.CacheCreationTokens
	out := map[string]interface{}{
		"prompt_tokens":     prompt,
		"completion_tokens": d.OutputTokens,
		"total_tokens":      prompt + d.OutputTokens,
	}
	if d.CacheReadTokens > 0 || d.CacheCreationTokens > 0 {
		out["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": d.CacheReadTokens}
	}
	return out
}

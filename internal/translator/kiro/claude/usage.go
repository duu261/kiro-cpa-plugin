package claude

import "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"

// ClaudeUsage renders a usage.Detail in the Anthropic Messages shape. Cache
// fields appear only when the turn was metered, so an unmetered turn keeps the
// plain {input_tokens, output_tokens} it always had.
func ClaudeUsage(detail usage.Detail) map[string]interface{} {
	out := map[string]interface{}{
		"input_tokens":  detail.InputTokens,
		"output_tokens": detail.OutputTokens,
	}
	if detail.CacheReadTokens > 0 || detail.CacheCreationTokens > 0 {
		out["cache_read_input_tokens"] = detail.CacheReadTokens
		out["cache_creation_input_tokens"] = detail.CacheCreationTokens
		out["cache_creation"] = map[string]interface{}{
			"ephemeral_5m_input_tokens": detail.CacheCreationTokens,
			"ephemeral_1h_input_tokens": int64(0),
		}
	}
	return out
}

// Package responses registers native OpenAI Responses translation for Kiro.
package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var registry = builtin.Registry()

func init() {
	kiroFormat := sdktranslator.FromString("kiro")
	sdktranslator.Register(
		sdktranslator.FormatOpenAIResponse,
		kiroFormat,
		convertResponsesRequestToClaude,
		sdktranslator.ResponseTransform{
			Stream: func(ctx context.Context, model string, originalRequest, request, response []byte, param *any) [][]byte {
				return convertClaudeStreamToResponses(ctx, model, originalRequest, request, response, param)
			},
			NonStream: func(ctx context.Context, model string, originalRequest, request, response []byte, param *any) []byte {
				return convertClaudeNonStreamToResponses(ctx, model, originalRequest, request, response)
			},
		},
	)
}

func convertClaudeStreamToResponses(ctx context.Context, model string, originalRequest, claudeRequest, response []byte, param *any) [][]byte {
	dataFrame := claudeDataFrame(response)
	if len(dataFrame) == 0 {
		return [][]byte{}
	}
	return registry.TranslateStream(
		ctx,
		sdktranslator.FormatClaude,
		sdktranslator.FormatOpenAIResponse,
		model,
		originalRequest,
		claudeRequest,
		dataFrame,
		param,
	)
}

func convertClaudeNonStreamToResponses(ctx context.Context, model string, originalRequest, claudeRequest, response []byte) []byte {
	response = claudeResponseSSE(response)
	return registry.TranslateNonStream(
		ctx,
		sdktranslator.FormatClaude,
		sdktranslator.FormatOpenAIResponse,
		model,
		originalRequest,
		claudeRequest,
		response,
		nil,
	)
}

func claudeDataFrame(frame []byte) []byte {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			return append([]byte(nil), line...)
		}
	}
	return nil
}

// claudeResponseSSE expresses a buffered Claude message as its canonical SSE
// event sequence. CLIProxyAPI's built-in Claude-to-Responses non-stream
// translator consumes that canonical representation and therefore preserves
// reasoning signatures, content ordering, and tool identities in one path for
// both streaming and buffered requests.
func claudeResponseSSE(response []byte) []byte {
	root := gjson.ParseBytes(response)
	var frames [][]byte
	appendFrame := func(value map[string]any) {
		encoded, err := json.Marshal(value)
		if err == nil {
			frames = append(frames, append([]byte("data: "), encoded...))
		}
	}

	message := map[string]any{
		"id":      root.Get("id").String(),
		"type":    "message",
		"role":    "assistant",
		"model":   root.Get("model").String(),
		"content": []any{},
		"usage":   claudeUsage(root.Get("usage"), false),
	}
	appendFrame(map[string]any{"type": "message_start", "message": message})

	for index, block := range root.Get("content").Array() {
		blockType := block.Get("type").String()
		var contentBlock map[string]any
		switch blockType {
		case "thinking":
			contentBlock = map[string]any{"type": "thinking", "thinking": ""}
		case "redacted_thinking":
			contentBlock = map[string]any{"type": "redacted_thinking", "data": block.Get("data").String()}
		case "tool_use":
			contentBlock = map[string]any{
				"type":  "tool_use",
				"id":    block.Get("id").String(),
				"name":  block.Get("name").String(),
				"input": map[string]any{},
			}
		default:
			contentBlock = map[string]any{"type": "text", "text": ""}
		}
		appendFrame(map[string]any{"type": "content_block_start", "index": index, "content_block": contentBlock})

		switch blockType {
		case "thinking":
			if text := block.Get("thinking").String(); text != "" {
				appendFrame(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "thinking_delta", "thinking": text}})
			}
			if signature := block.Get("signature").String(); signature != "" {
				appendFrame(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "signature_delta", "signature": signature}})
			}
		case "tool_use":
			input := block.Get("input").Raw
			if strings.TrimSpace(input) == "" {
				input = "{}"
			}
			appendFrame(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": input}})
		case "text":
			if text := block.Get("text").String(); text != "" {
				appendFrame(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}})
			}
		}
		appendFrame(map[string]any{"type": "content_block_stop", "index": index})
	}

	appendFrame(map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   root.Get("stop_reason").String(),
			"stop_sequence": nil,
		},
		"usage": claudeUsage(root.Get("usage"), true),
	})
	appendFrame(map[string]any{"type": "message_stop"})
	return bytes.Join(frames, []byte("\n"))
}

// claudeUsage carries a buffered message's usage into its SSE frames, cache
// fields included, so a metered turn keeps its cache split on the Responses
// path. message_start reports no output yet, as a live stream would.
func claudeUsage(u gjson.Result, withOutput bool) map[string]any {
	out := map[string]any{
		"input_tokens":  u.Get("input_tokens").Int(),
		"output_tokens": int64(0),
	}
	if withOutput {
		out["output_tokens"] = u.Get("output_tokens").Int()
	}
	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if v := u.Get(key); v.Exists() {
			out[key] = v.Int()
		}
	}
	if v := u.Get("cache_creation"); v.IsObject() {
		out["cache_creation"] = v.Value()
	}
	return out
}

func convertResponsesRequestToClaude(model string, body []byte, stream bool) []byte {
	body = normalizeResponsesStringInput(body)
	return registry.TranslateRequest(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude,
		model,
		body,
		stream,
	)
}

func normalizeResponsesStringInput(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if input.Type != gjson.String {
		return body
	}
	message := []byte(`[{"role":"user","content":[{"type":"input_text","text":""}]}]`)
	message, err := sjson.SetBytes(message, "0.content.0.text", input.String())
	if err != nil {
		return body
	}
	normalized, err := sjson.SetRawBytes(body, "input", message)
	if err != nil {
		return body
	}
	return normalized
}

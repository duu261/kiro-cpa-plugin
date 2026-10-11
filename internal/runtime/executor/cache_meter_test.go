package executor

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/nguyenha935/kiro-cpa-plugin/internal/cachemeter"
	"github.com/nguyenha935/kiro-cpa-plugin/internal/modelcapabilities"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// 22.5% of a 200000-token window is 45000 tokens; the opus-5.5 hidden prompt
// (6690) comes off, minus the locally counted output "hello".
const meterWindow = 200000

func meterBody(format string) []byte {
	long := strings.Repeat("lorem ipsum dolor sit amet consectetur ", 600)
	if format == "openai" {
		return []byte(`{"model":"claude-opus-5.5","max_tokens":16,"messages":[{"role":"system","content":"` + long + `"},{"role":"user","content":"` + long + `"}]}`)
	}
	return []byte(`{"model":"claude-opus-5.5","max_tokens":16,"system":"` + long + `","messages":[{"role":"user","content":"` + long + `"}]}`)
}

func meterRequest(format string, stream bool) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	body := meterBody(format)
	f := sdktranslator.FromString(format)
	return cliproxyexecutor.Request{Model: "claude-opus-5.5", Payload: body, Format: f},
		cliproxyexecutor.Options{SourceFormat: f, OriginalRequest: body, Stream: stream}
}

func setupMeterTest(t *testing.T, authID string) {
	t.Helper()
	cachemeter.Configure(cachemeter.Config{Enabled: true, ReadRatio: 0.88, WriteRatio: 0.88})
	t.Cleanup(func() { cachemeter.Configure(cachemeter.Config{}) })
	modelcapabilities.ReplaceForAuth(authID, []modelcapabilities.Capability{{ModelID: "claude-opus-5.5", InputTokenLimit: meterWindow}})
	t.Cleanup(func() { modelcapabilities.ReplaceForAuth(authID, nil) })
	body := string(kiroEventStream(
		statusEvent("assistantResponseEvent", `{"content":"hello"}`),
		statusEvent("metadataEvent", `{"stopReason":"END_TURN"}`),
		statusEvent("contextUsageEvent", `{"contextUsagePercentage":22.5}`),
	))
	stubUpstream(t, func(*http.Request) (*http.Response, error) {
		return upstreamStatus(http.StatusOK, nil, body), nil
	})
}

func wantPrompt() int64 { return 45000 - estimateKiroTokens("hello") - 6690 }

func ratioOf(prompt int64) int64 { return int64(float64(prompt) * 0.88) }

// Claude clients: the first turn writes 88% of the prompt, the identical
// second turn reads 88%, the rest is plain input, and read+write+input equals
// the real prompt.
func TestCacheMeterClaudeNonStream(t *testing.T) {
	auth := builderIDAuth("meter-claude.json")
	setupMeterTest(t, auth.ID)
	prompt := wantPrompt()
	for turn, want := range [][2]int64{{0, ratioOf(prompt)}, {ratioOf(prompt), 0}} {
		req, opts := meterRequest("claude", false)
		resp, err := NewKiroExecutor(nil).Execute(t.Context(), auth, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		u := gjson.GetBytes(resp.Payload, "usage")
		read, write, input := u.Get("cache_read_input_tokens").Int(), u.Get("cache_creation_input_tokens").Int(), u.Get("input_tokens").Int()
		if read != want[0] || write != want[1] || read+write+input != prompt {
			t.Fatalf("turn %d usage %s, want read %d write %d of prompt %d", turn+1, u.Raw, want[0], want[1], prompt)
		}
		if u.Get("cache_creation.ephemeral_5m_input_tokens").Int() != write {
			t.Fatalf("turn %d 5m write breakdown missing: %s", turn+1, u.Raw)
		}
	}
}

func TestCacheMeterClaudeStream(t *testing.T) {
	auth := builderIDAuth("meter-claude-stream.json")
	setupMeterTest(t, auth.ID)
	prompt := wantPrompt()
	for turn, want := range [][2]int64{{0, ratioOf(prompt)}, {ratioOf(prompt), 0}} {
		req, opts := meterRequest("claude", true)
		result, err := NewKiroExecutor(nil).ExecuteStream(t.Context(), auth, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		var delta gjson.Result
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			if d := eventData(chunk.Payload); d.Get("type").String() == "message_delta" {
				delta = d.Get("usage")
			}
		}
		read, write, input := delta.Get("cache_read_input_tokens").Int(), delta.Get("cache_creation_input_tokens").Int(), delta.Get("input_tokens").Int()
		if read != want[0] || write != want[1] || read+write+input != prompt {
			t.Fatalf("turn %d message_delta usage %s, want read %d write %d of prompt %d", turn+1, delta.Raw, want[0], want[1], prompt)
		}
	}
}

// OpenAI clients see the whole prompt in prompt_tokens and the read part in
// prompt_tokens_details.cached_tokens, on both paths.
func TestCacheMeterOpenAI(t *testing.T) {
	auth := builderIDAuth("meter-openai.json")
	setupMeterTest(t, auth.ID)
	prompt := wantPrompt()
	wantRead := ratioOf(prompt)

	req, opts := meterRequest("openai", false)
	if _, err := NewKiroExecutor(nil).Execute(t.Context(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	req, opts = meterRequest("openai", false)
	resp, err := NewKiroExecutor(nil).Execute(t.Context(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	u := gjson.GetBytes(resp.Payload, "usage")
	if u.Get("prompt_tokens").Int() != prompt || u.Get("prompt_tokens_details.cached_tokens").Int() != wantRead {
		t.Fatalf("non-stream OpenAI usage %s, want prompt %d cached %d", u.Raw, prompt, wantRead)
	}

	req, opts = meterRequest("openai", true)
	result, err := NewKiroExecutor(nil).ExecuteStream(t.Context(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var got gjson.Result
	for chunk := range result.Chunks {
		if v := gjson.GetBytes(bytes.TrimSpace(chunk.Payload), "usage"); v.Exists() {
			got = v
		}
	}
	if got.Get("prompt_tokens").Int() != prompt || got.Get("prompt_tokens_details.cached_tokens").Int() != wantRead {
		t.Fatalf("stream OpenAI usage %s, want prompt %d cached %d", got.Raw, prompt, wantRead)
	}
}

// With the meter off, output is exactly what the plugin produced before.
func TestCacheMeterOffKeepsPlainUsage(t *testing.T) {
	auth := builderIDAuth("meter-off.json")
	setupMeterTest(t, auth.ID)
	cachemeter.Configure(cachemeter.Config{})
	req, opts := meterRequest("claude", false)
	resp, err := NewKiroExecutor(nil).Execute(t.Context(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	u := gjson.GetBytes(resp.Payload, "usage")
	if u.Get("cache_read_input_tokens").Exists() || u.Get("input_tokens").Int() != 45000-estimateKiroTokens("hello") {
		t.Fatalf("meter off changed usage: %s", u.Raw)
	}
}

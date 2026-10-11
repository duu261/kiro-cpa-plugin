package cachemeter

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func body(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func longText(seed string) string {
	return strings.Repeat(seed+" lorem ipsum dolor sit amet consectetur ", 400)
}

func conversation(t *testing.T, turns int) []byte {
	t.Helper()
	msgs := []any{}
	for i := 0; i < turns; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": longText("user" + string(rune('a'+i)))})
		if i < turns-1 {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "reply " + string(rune('a'+i))})
		}
	}
	return body(t, map[string]any{
		"model":    "claude-opus-5-5",
		"system":   []any{map[string]any{"type": "text", "text": longText("system")}},
		"messages": msgs,
	})
}

func newTestMeter(ratio float64) (*Meter, *time.Time) {
	m := New(Config{Enabled: true, ReadRatio: ratio, WriteRatio: 0.88})
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	return m, &now
}

const model = "claude-opus-5.5"

func detailFor(promptTokens int64) usage.Detail {
	in := promptTokens + hiddenPromptTokens[model]
	return usage.Detail{InputTokens: in, OutputTokens: 7, TotalTokens: in + 7}
}

func sum(d usage.Detail) int64 { return d.InputTokens + d.CacheReadTokens + d.CacheCreationTokens }

// A miss writes 88% and bills the rest as plain input, never reads.
func TestFirstTurnWritesWithInputSlice(t *testing.T) {
	m, _ := newTestMeter(0.88)
	got := m.Apply("cred", model, conversation(t, 1), detailFor(20000))
	if got.CacheReadTokens != 0 || got.CacheCreationTokens != 17600 || got.InputTokens != 2400 {
		t.Fatalf("first turn = %+v", got)
	}
	if sum(got) != 20000 {
		t.Fatalf("prompt total %d, want 20000 (hidden Kiro prompt removed)", sum(got))
	}
	if got.TotalTokens != 20007 || got.OutputTokens != 7 {
		t.Fatalf("total/output = %d/%d", got.TotalTokens, got.OutputTokens)
	}
}

// A hit reads up to the ratio and bills the rest as plain input, never writes.
func TestIdenticalRepeatIsCappedRead(t *testing.T) {
	m, _ := newTestMeter(0.88)
	req := conversation(t, 1)
	m.Apply("cred", model, req, detailFor(20000))
	got := m.Apply("cred", model, req, detailFor(20000))
	if got.CacheReadTokens != 17600 || got.CacheCreationTokens != 0 || got.InputTokens != 2400 || sum(got) != 20000 {
		t.Fatalf("repeat = %+v", got)
	}
	if got.CachedTokens != got.CacheReadTokens {
		t.Fatalf("CachedTokens %d != CacheReadTokens %d", got.CachedTokens, got.CacheReadTokens)
	}
}

func TestNextTurnReadsPreviousPrefix(t *testing.T) {
	m, _ := newTestMeter(0.95)
	m.Apply("cred", model, conversation(t, 2), detailFor(30000))
	got := m.Apply("cred", model, conversation(t, 3), detailFor(45000))
	share := float64(got.CacheReadTokens) / 45000
	if share < 0.55 || share > 0.75 {
		t.Fatalf("turn 3 read share %.3f, want about 2/3 of the prompt: %+v", share, got)
	}
	if sum(got) != 45000 || got.CacheCreationTokens != 0 || got.InputTokens != 45000-got.CacheReadTokens {
		t.Fatalf("turn 3 = %+v", got)
	}
}

func TestTTLExpiry(t *testing.T) {
	m, now := newTestMeter(0.8)
	req := conversation(t, 1)
	m.Apply("cred", model, req, detailFor(20000))
	*now = now.Add(TTL + time.Second)
	if got := m.Apply("cred", model, req, detailFor(20000)); got.CacheReadTokens != 0 {
		t.Fatalf("expired prefix still read: %+v", got)
	}
}

func TestHitRenewsTTL(t *testing.T) {
	m, now := newTestMeter(0.8)
	req := conversation(t, 1)
	m.Apply("cred", model, req, detailFor(20000))
	*now = now.Add(4 * time.Minute)
	m.Apply("cred", model, req, detailFor(20000))
	*now = now.Add(4 * time.Minute)
	if got := m.Apply("cred", model, req, detailFor(20000)); got.CacheReadTokens == 0 {
		t.Fatalf("hit did not renew TTL: %+v", got)
	}
}

func TestScopedPerCredentialAndModel(t *testing.T) {
	m, _ := newTestMeter(0.8)
	req := conversation(t, 1)
	m.Apply("cred-a", model, req, detailFor(20000))
	if got := m.Apply("cred-b", model, req, detailFor(20000)); got.CacheReadTokens != 0 {
		t.Fatalf("other credential read a's cache: %+v", got)
	}
	other := "claude-sonnet-5"
	d := usage.Detail{InputTokens: 20000 + hiddenPromptTokens[other]}
	if got := m.Apply("cred-a", other, req, d); got.CacheReadTokens != 0 {
		t.Fatalf("other model read opus cache: %+v", got)
	}
}

func TestVolatileFieldsDoNotBreakPrefix(t *testing.T) {
	m, _ := newTestMeter(0.8)
	first := body(t, map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.0; cch=aaaa;"},
			map[string]any{"type": "text", "text": longText("system"), "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": longText("u"), "cache_control": map[string]any{"type": "ephemeral"}},
		}}},
	})
	second := body(t, map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.0; cch=bbbb;"},
			map[string]any{"type": "text", "text": longText("system")},
		},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": longText("u")},
		}}},
	})
	m.Apply("cred", model, first, detailFor(20000))
	if got := m.Apply("cred", model, second, detailFor(20000)); got.CacheReadTokens != 16000 {
		t.Fatalf("billing header / cache_control changed the fingerprint: %+v", got)
	}
}

func TestShortPrefixIsNotCached(t *testing.T) {
	m, _ := newTestMeter(0.88)
	req := body(t, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	m.Apply("cred", model, req, detailFor(30))
	if got := m.Apply("cred", model, req, detailFor(30)); got.CacheReadTokens != 0 || got.CacheCreationTokens != 0 || got.InputTokens != 30 {
		t.Fatalf("prompt under the minimum not billed as plain input: %+v", got)
	}
}

func TestDisabledAndNonClaudeUnchanged(t *testing.T) {
	in := detailFor(20000)
	off := New(Config{})
	if got := off.Apply("cred", model, conversation(t, 1), in); got != in {
		t.Fatalf("disabled meter changed usage: %+v", got)
	}
	m, _ := newTestMeter(0.8)
	gpt := usage.Detail{InputTokens: 5000, OutputTokens: 3}
	if got := m.Apply("cred", "gpt-5.6-sol", conversation(t, 1), gpt); got != gpt {
		t.Fatalf("non-Claude model changed: %+v", got)
	}
	zero := usage.Detail{OutputTokens: 3}
	if got := m.Apply("cred", model, conversation(t, 1), zero); got != zero {
		t.Fatalf("turn without input count changed: %+v", got)
	}
}

func TestUnknownClaudeModelKeepsFullTotal(t *testing.T) {
	m, _ := newTestMeter(0.8)
	got := m.Apply("cred", "claude-future-9", conversation(t, 1), usage.Detail{InputTokens: 12345})
	if sum(got) != 12345 {
		t.Fatalf("unknown model total %d, want 12345", sum(got))
	}
}

func TestSplit(t *testing.T) {
	cases := []struct {
		total             int64
		share, rr, wr     float64
		read, write, inpt int64
	}{
		{10000, 0, 0.88, 0.88, 0, 8800, 1200},   // miss: write + input slice
		{10000, 1, 0.88, 0.88, 8800, 0, 1200},   // full hit capped at read ratio
		{10000, 0.5, 0.88, 0.88, 5000, 0, 5000}, // partial hit: uncached suffix is input
		{10000, 1, 1, 1, 10000, 0, 0},
		{10000, 0, 0.88, 1, 0, 10000, 0},
		{1023, 1, 0.88, 0.88, 0, 0, 1023}, // under the cacheable minimum
		{1, 0, 0.88, 0.88, 0, 0, 1},
		{0, 1, 0.88, 0.88, 0, 0, 0},
	}
	for _, c := range cases {
		r, w, i := Split(c.total, c.share, c.rr, c.wr)
		if r != c.read || w != c.write || i != c.inpt || (c.total > 0 && r+w+i != c.total) || (r > 0 && w > 0) {
			t.Fatalf("Split(%d,%v,%v,%v) = %d/%d/%d, want %d/%d/%d", c.total, c.share, c.rr, c.wr, r, w, i, c.read, c.write, c.inpt)
		}
	}
}

func TestConfigureRejectsBadRatio(t *testing.T) {
	for _, r := range []float64{0, -1, 1.5, math.NaN()} {
		m := New(Config{Enabled: true, ReadRatio: r, WriteRatio: r})
		if m.cfg.ReadRatio != DefaultReadRatio || m.cfg.WriteRatio != DefaultWriteRatio {
			t.Fatalf("ratio %v kept as %v/%v", r, m.cfg.ReadRatio, m.cfg.WriteRatio)
		}
	}
}

func TestImageDataIsDigested(t *testing.T) {
	img := strings.Repeat("A", 200000)
	blocks := requestBlocks(body(t, map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": img}},
	}}}}))
	if len(blocks) != 1 || strings.Contains(blocks[0], img[:100]) || !strings.Contains(blocks[0], imageMarker) {
		t.Fatalf("image not digested: %d bytes", len(blocks[0]))
	}
	if n := countTokens(blocks[0]); n < imageTokens || n > imageTokens+200 {
		t.Fatalf("image block counted as %d tokens", n)
	}
}

func TestOpenAIBodiesAreMetered(t *testing.T) {
	m, _ := newTestMeter(0.8)
	req := body(t, map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": longText("sys")},
		map[string]any{"role": "user", "content": longText("u")},
	}})
	m.Apply("cred", model, req, detailFor(20000))
	if got := m.Apply("cred", model, req, detailFor(20000)); got.CacheReadTokens != 16000 || got.InputTokens != 4000 {
		t.Fatalf("OpenAI chat body not metered: %+v", got)
	}
}

func TestMemoryBound(t *testing.T) {
	m, _ := newTestMeter(0.8)
	for i := 0; i < maxEntries/2+10; i++ {
		m.lookupAndRecord([][32]byte{{byte(i), byte(i >> 8), byte(i >> 16)}, {byte(i), byte(i >> 8), byte(i >> 16), 1}})
	}
	if len(m.entries) > maxEntries {
		t.Fatalf("entries %d exceed bound %d", len(m.entries), maxEntries)
	}
	long := make([][32]byte, maxEntries+5)
	for i := range long {
		long[i] = [32]byte{9, byte(i), byte(i >> 8), byte(i >> 16)}
	}
	m.lookupAndRecord(long)
	if len(m.entries) > maxEntries {
		t.Fatalf("one long request grew entries to %d, bound %d", len(m.entries), maxEntries)
	}
	if _, ok := m.entries[long[len(long)-1]]; !ok {
		t.Fatal("longest prefix of the long request was not kept")
	}
}

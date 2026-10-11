// Package cachemeter reports Anthropic-style prompt-cache usage for Kiro.
//
// Kiro bills credits per turn and never reports prompt caching, so a client
// that is billed on Anthropic semantics sees every turn as full-price input.
// This package simulates the cache split a Claude client would see:
//
//   - The prompt total comes from Kiro itself (contextUsagePercentage times the
//     model's window), minus the fixed hidden system prompt Kiro adds per model.
//     That makes the total match Anthropic's own count_tokens for the request.
//   - The request is cut into blocks (each tool, the system prompt, each
//     message). A hash chain over the blocks gives one fingerprint per prefix.
//     A prefix seen within the TTL on the same credential is a cache hit; its
//     share of the prompt is measured with a local tokenizer and applied to
//     Kiro's exact total.
//   - A hit reads the cached prefix, capped at ReadRatio of the total; the
//     rest is uncached input. A miss writes WriteRatio of the total; the rest
//     is uncached input. A prompt under the cacheable minimum is all input.
//     This is the shape Kiro resellers bill on (about 12% input either way),
//     not Anthropic's, where input is only the suffix after the last marker.
//
// The split only changes how usage is reported; Kiro is called the same way.
package cachemeter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

const (
	// DefaultReadRatio caps simulated cache reads as a share of the prompt on
	// a hit; DefaultWriteRatio is the share written on a miss. The remainder
	// of either is billed as uncached input.
	DefaultReadRatio  = 0.88
	DefaultWriteRatio = 0.88
	// TTL matches Anthropic's default ephemeral cache, renewed on every hit.
	TTL = 5 * time.Minute
	// minCacheableTokens mirrors Anthropic's minimum cacheable prefix.
	minCacheableTokens = 1024
	// maxEntries bounds memory.
	maxEntries = 200000
	// imageTokens is a flat local estimate per image; only proportions use it.
	imageTokens         = 1600
	imageMarker         = "image-sha256:"
	billingHeaderPrefix = "x-anthropic-billing-header:"
)

// hiddenPromptTokens is the prompt Kiro adds to every request, measured on
// 2026-10-11 as Kiro's input for a minimal request minus Anthropic's
// count_tokens for the same request. It is identical on repeated requests.
var hiddenPromptTokens = map[string]int64{
	"claude-opus-5.5":   6690,
	"claude-sonnet-5.5": 6501,
	"claude-opus-5":     6688,
	"claude-sonnet-5":   6499,
	"claude-opus-4.8":   6413,
	"claude-opus-4.7":   5899,
	"claude-opus-4.6":   4096,
	"claude-sonnet-4.6": 4035,
	"claude-opus-4.5":   4034,
	"claude-sonnet-4.5": 4034,
	"claude-sonnet-4":   4034,
	"claude-haiku-4.5":  4034,
}

// Config controls the meter. The zero value disables it.
type Config struct {
	Enabled    bool
	ReadRatio  float64
	WriteRatio float64
}

// Meter holds prefix fingerprints seen within the TTL.
type Meter struct {
	mu      sync.Mutex
	cfg     Config
	entries map[[32]byte]time.Time
	now     func() time.Time
}

// New returns a meter with the given config.
func New(cfg Config) *Meter {
	m := &Meter{entries: make(map[[32]byte]time.Time), now: time.Now}
	m.Configure(cfg)
	return m
}

var global = New(Config{})

// Configure replaces the global meter's settings. Recorded prefixes are kept.
func Configure(cfg Config) { global.Configure(cfg) }

// Apply meters one completed turn on the global meter.
func Apply(scope, kiroModel string, request []byte, detail usage.Detail) usage.Detail {
	return global.Apply(scope, kiroModel, request, detail)
}

// Configure replaces this meter's settings.
func (m *Meter) Configure(cfg Config) {
	if !validRatio(cfg.ReadRatio) {
		cfg.ReadRatio = DefaultReadRatio
	}
	if !validRatio(cfg.WriteRatio) {
		cfg.WriteRatio = DefaultWriteRatio
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// Apply rewrites detail into an Anthropic cache split for a successful turn
// and records the request's prefixes under scope (the Kiro credential, as
// Anthropic scopes its cache per organization). A disabled meter, a non-Claude
// model and a turn with no input count are returned unchanged.
func (m *Meter) Apply(scope, kiroModel string, request []byte, detail usage.Detail) usage.Detail {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if !cfg.Enabled || detail.InputTokens <= 0 || !strings.HasPrefix(kiroModel, "claude-") {
		return detail
	}
	total := detail.InputTokens - hiddenPromptTokens[kiroModel]
	if total < 1 {
		total = 1
	}

	chain, counts := prefixChain(scope, kiroModel, request)
	matched := m.lookupAndRecord(chain)

	var share float64
	if last := counts[len(counts)-1]; matched >= 0 && last > 0 && counts[matched] >= minCacheableTokens {
		share = float64(counts[matched]) / float64(last)
	}
	read, write, input := Split(total, share, cfg.ReadRatio, cfg.WriteRatio)

	detail.InputTokens = input
	detail.CacheReadTokens = read
	detail.CachedTokens = read
	detail.CacheCreationTokens = write
	detail.TotalTokens = input + read + write + detail.OutputTokens
	return detail
}

// Split divides total prompt tokens into cache read, cache write and uncached
// input. share is the cached prefix's fraction of the prompt (0 on a miss).
// A hit never writes and a miss never reads, so read+write+input == total.
func Split(total int64, share, readRatio, writeRatio float64) (read, write, input int64) {
	switch {
	case total <= 0:
		return 0, 0, 0
	case total < minCacheableTokens:
		return 0, 0, total
	case share > 0:
		read = int64(math.Floor(math.Min(share, readRatio) * float64(total)))
		return read, 0, total - read
	default:
		write = int64(math.Floor(writeRatio * float64(total)))
		return 0, write, total - write
	}
}

func validRatio(r float64) bool { return !math.IsNaN(r) && r > 0 && r <= 1 }

// lookupAndRecord returns the index of the longest live recorded prefix (or
// -1) and records every prefix of this request with a fresh TTL.
func (m *Meter) lookupAndRecord(chain [][32]byte) int {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	matched := -1
	for i := len(chain) - 1; i >= 0; i-- {
		if expires, ok := m.entries[chain[i]]; ok && now.Before(expires) {
			matched = i
			break
		}
	}
	if len(m.entries)+len(chain) > maxEntries {
		m.pruneLocked(now)
	}
	// A single request longer than the whole bound keeps only its longest
	// prefixes; earlier ones would be evicted first anyway.
	if len(chain) > maxEntries {
		chain = chain[len(chain)-maxEntries:]
	}
	if len(m.entries)+len(chain) > maxEntries {
		m.entries = make(map[[32]byte]time.Time)
	}
	for _, h := range chain {
		m.entries[h] = now.Add(TTL)
	}
	return matched
}

// pruneLocked drops expired entries; if that frees too little, it drops the
// entries closest to expiry, and as a last resort everything.
func (m *Meter) pruneLocked(now time.Time) {
	for h, expires := range m.entries {
		if !now.Before(expires) {
			delete(m.entries, h)
		}
	}
	if len(m.entries) <= maxEntries*3/4 {
		return
	}
	cutoff := now.Add(TTL / 2)
	for h, expires := range m.entries {
		if expires.Before(cutoff) {
			delete(m.entries, h)
		}
	}
	if len(m.entries) > maxEntries*3/4 {
		m.entries = make(map[[32]byte]time.Time)
	}
}

// prefixChain hashes the request block by block. chain[i] fingerprints blocks
// 0..i and counts[i] is their cumulative local token count. Scope and model
// seed the chain because caches are per credential and per model.
func prefixChain(scope, model string, request []byte) ([][32]byte, []int64) {
	blocks := requestBlocks(request)
	chain := make([][32]byte, 0, len(blocks)+1)
	counts := make([]int64, 0, len(blocks)+1)
	prev := sha256.Sum256([]byte("kiro-cache-v1\x00" + scope + "\x00" + model))
	var cumulative int64
	for _, b := range blocks {
		h := sha256.New()
		h.Write(prev[:])
		h.Write([]byte(b))
		copy(prev[:], h.Sum(nil))
		cumulative += countTokens(b)
		chain = append(chain, prev)
		counts = append(counts, cumulative)
	}
	if len(chain) == 0 {
		chain = append(chain, prev)
		counts = append(counts, 0)
	}
	return chain, counts
}

// requestBlocks returns canonical blocks in prompt order: each tool, the
// system prompt, then each message. It accepts Claude and OpenAI chat bodies.
// cache_control markers and Claude Code's per-request billing header are
// removed, because they change between turns without changing the prompt.
func requestBlocks(request []byte) []string {
	root := gjson.ParseBytes(request)
	var blocks []string
	for _, tool := range root.Get("tools").Array() {
		blocks = append(blocks, "tool:"+canonical(tool.Raw))
	}
	if system := root.Get("system"); system.Exists() {
		if system.IsArray() {
			var parts []string
			for _, part := range system.Array() {
				if isBillingHeader(part.Get("text").String()) {
					continue
				}
				parts = append(parts, canonical(part.Raw))
			}
			if len(parts) > 0 {
				blocks = append(blocks, "system:"+strings.Join(parts, ","))
			}
		} else if text := system.String(); text != "" && !isBillingHeader(text) {
			blocks = append(blocks, "system:"+canonical(system.Raw))
		}
	}
	for _, msg := range root.Get("messages").Array() {
		blocks = append(blocks, "msg:"+msg.Get("role").String()+":"+canonical(msg.Get("content").Raw))
	}
	return blocks
}

func isBillingHeader(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), billingHeaderPrefix)
}

// canonical re-encodes JSON with sorted keys, without cache_control, and with
// inline image data replaced by its digest.
func canonical(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	out, err := json.Marshal(normalize(v))
	if err != nil {
		return raw
	}
	return string(out)
}

func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		if t["type"] == "base64" {
			if data, ok := t["data"].(string); ok {
				t["data"] = digest(data)
			}
		}
		for k, child := range t {
			t[k] = normalize(child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = normalize(child)
		}
		return t
	case string:
		// OpenAI image_url data URLs.
		if strings.HasPrefix(t, "data:image/") {
			return digest(t)
		}
		return t
	default:
		return v
	}
}

func digest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return imageMarker + hex.EncodeToString(sum[:])
}

var (
	codecOnce sync.Once
	codec     tokenizer.Codec
)

// countTokens is a local estimate used only for prefix proportions; the
// absolute total always comes from Kiro.
func countTokens(block string) int64 {
	images := int64(strings.Count(block, imageMarker))
	codecOnce.Do(func() { codec, _ = tokenizer.Get(tokenizer.O200kBase) })
	n := int64(len(block) / 4)
	if codec != nil {
		if c, err := codec.Count(block); err == nil {
			n = int64(c)
		}
	}
	return n + images*imageTokens
}

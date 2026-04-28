package agent

import (
	"math"
	"testing"

	"ollie/pkg/backend"
)

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// TestComputeCostUSD_BaseTokens verifies input+output pricing with no cache fields.
func TestComputeCostUSD_BaseTokens(t *testing.T) {
	// claude-sonnet-4: $3/M input, $15/M output
	got := computeCostUSD("claude-sonnet-4-5", backend.Usage{
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	})
	want := 3.00 + 15.00
	if !approxEqual(got, want, 0.001) {
		t.Errorf("got %.4f; want %.4f", got, want)
	}
}

// TestComputeCostUSD_ClaudeCacheReadDiscount verifies that cached input tokens
// are charged at 10% of the normal input rate for Claude models.
func TestComputeCostUSD_ClaudeCacheReadDiscount(t *testing.T) {
	// claude-sonnet-4: $3/M input → cache read = $0.30/M
	got := computeCostUSD("claude-sonnet-4-5", backend.Usage{
		InputTokens:       0,
		CachedInputTokens: 1_000_000,
		OutputTokens:      0,
	})
	want := 0.30 // 10% of $3.00
	if !approxEqual(got, want, 0.001) {
		t.Errorf("claude cache read: got %.4f; want %.4f", got, want)
	}
}

// TestComputeCostUSD_NonClaudeCacheReadDiscount verifies that cached input tokens
// are charged at 50% of the normal input rate for non-Claude models (OpenAI).
func TestComputeCostUSD_NonClaudeCacheReadDiscount(t *testing.T) {
	// gpt-4o: $2.50/M input → cache read = $1.25/M
	got := computeCostUSD("gpt-4o", backend.Usage{
		InputTokens:       0,
		CachedInputTokens: 1_000_000,
		OutputTokens:      0,
	})
	want := 1.25 // 50% of $2.50
	if !approxEqual(got, want, 0.001) {
		t.Errorf("gpt-4o cache read: got %.4f; want %.4f", got, want)
	}
}

// TestComputeCostUSD_CacheCreationSurcharge verifies cache creation tokens are
// charged at 125% of the normal input rate.
func TestComputeCostUSD_CacheCreationSurcharge(t *testing.T) {
	// claude-sonnet-4: $3/M input → cache creation = $3.75/M
	got := computeCostUSD("claude-sonnet-4-5", backend.Usage{
		InputTokens:         0,
		CacheCreationTokens: 1_000_000,
		OutputTokens:        0,
	})
	want := 3.75 // 125% of $3.00
	if !approxEqual(got, want, 0.001) {
		t.Errorf("cache creation surcharge: got %.4f; want %.4f", got, want)
	}
}

// TestComputeCostUSD_AllFieldsCombined verifies that all four token categories
// are summed correctly in a single call.
func TestComputeCostUSD_AllFieldsCombined(t *testing.T) {
	// claude-sonnet-4: $3/M in, $15/M out
	//   100k normal input  = $0.30
	//   200k cached input  = $0.06   (10% of $3/M)
	//   50k  cache create  = $0.1875 (125% of $3/M)
	//   100k output        = $1.50
	got := computeCostUSD("claude-sonnet-4-5", backend.Usage{
		InputTokens:         100_000,
		CachedInputTokens:   200_000,
		CacheCreationTokens: 50_000,
		OutputTokens:        100_000,
	})
	want := 0.30 + 0.06 + 0.1875 + 1.50
	if !approxEqual(got, want, 0.0001) {
		t.Errorf("combined: got %.6f; want %.6f", got, want)
	}
}

// TestComputeCostUSD_UnknownModelZero verifies that unknown/local models return 0.
func TestComputeCostUSD_UnknownModelZero(t *testing.T) {
	got := computeCostUSD("llama-3-local", backend.Usage{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if got != 0 {
		t.Errorf("unknown model: got %.4f; want 0", got)
	}
}

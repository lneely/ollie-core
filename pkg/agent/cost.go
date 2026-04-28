package agent

import (
	"strings"

	"ollie/pkg/backend"
)

// auditTruncate trims s to 200 runes for log output.
func auditTruncate(s string) string {
	runes := []rune(s)
	if len(runes) <= 200 {
		return s
	}
	return string(runes[:200]) + "…"
}

// modelPrice holds USD per 1M input and output tokens for a model family.
type modelPrice struct {
	inputPer1M  float64
	outputPer1M float64
}

// modelPrices maps case-insensitive model name prefixes to USD/1M token prices.
// More specific prefixes must appear before more general ones.
// Source: public API pricing pages. Local/unknown models return zero cost.
var modelPrices = []struct {
	prefix string
	price  modelPrice
}{
	// Anthropic
	{"claude-opus-4", modelPrice{15.00, 75.00}},
	{"claude-sonnet-4", modelPrice{3.00, 15.00}},
	{"claude-haiku-4", modelPrice{0.80, 4.00}},
	{"claude-3-opus", modelPrice{15.00, 75.00}},
	{"claude-3-5-sonnet", modelPrice{3.00, 15.00}},
	{"claude-3-5-haiku", modelPrice{0.80, 4.00}},
	{"claude-3-sonnet", modelPrice{3.00, 15.00}},
	{"claude-3-haiku", modelPrice{0.25, 1.25}},
	// OpenAI — specific before general
	{"gpt-4o-mini", modelPrice{0.15, 0.60}},
	{"gpt-4o", modelPrice{2.50, 10.00}},
	{"gpt-4-turbo", modelPrice{10.00, 30.00}},
	{"o1-mini", modelPrice{3.00, 12.00}},
	{"o3-mini", modelPrice{1.10, 4.40}},
	{"o1", modelPrice{15.00, 60.00}},
	{"o3", modelPrice{10.00, 40.00}},
	// Google Gemini
	{"gemini-2.5-pro", modelPrice{1.25, 10.00}},
	{"gemini-2.5-flash", modelPrice{0.15, 0.60}},
	{"gemini-2.0-flash", modelPrice{0.10, 0.40}},
	{"gemini-1.5-pro", modelPrice{1.25, 5.00}},
	{"gemini-1.5-flash", modelPrice{0.075, 0.30}},
}

// computeCostUSD returns the cost in USD for the given usage and model.
// CachedInputTokens are NOT included in u.InputTokens; they are charged at a
// discount (10% for Anthropic, 50% for other providers).
// CacheCreationTokens are charged at 125% of the normal input rate (Anthropic).
// Returns 0 for unknown/local models.
func computeCostUSD(model string, u backend.Usage) float64 {
	lower := strings.ToLower(model)
	for _, entry := range modelPrices {
		if strings.HasPrefix(lower, entry.prefix) {
			inRate := entry.price.inputPer1M / 1e6
			outRate := entry.price.outputPer1M / 1e6
			cost := float64(u.InputTokens)*inRate + float64(u.OutputTokens)*outRate
			if u.CachedInputTokens > 0 {
				cacheReadRate := 0.50
				if strings.HasPrefix(lower, "claude-") {
					cacheReadRate = 0.10
				}
				cost += float64(u.CachedInputTokens) * inRate * cacheReadRate
			}
			if u.CacheCreationTokens > 0 {
				cost += float64(u.CacheCreationTokens) * inRate * 1.25
			}
			return cost
		}
	}
	return 0
}

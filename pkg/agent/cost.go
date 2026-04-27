package agent

import "strings"

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

// computeCostUSD returns the cost in USD for the given token counts and model.
// Returns 0 for unknown/local models.
func computeCostUSD(model string, inputTokens, outputTokens int) float64 {
	lower := strings.ToLower(model)
	for _, entry := range modelPrices {
		if strings.HasPrefix(lower, entry.prefix) {
			return float64(inputTokens)*entry.price.inputPer1M/1e6 +
				float64(outputTokens)*entry.price.outputPer1M/1e6
		}
	}
	return 0
}

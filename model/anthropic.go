package model

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// fallbackModels accept server-side refusal fallbacks on the Claude API.
var fallbackModels = map[string]bool{
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-sonnet-5-5": true,
}

type anthropicModel struct {
	client    anthropic.Client
	model     string
	fallbacks bool
}

// newAnthropic leaves credentials to the SDK when Config has none, so
// ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL, and a signed-in profile all work.
func newAnthropic(cfg Config) *anthropicModel {
	opts := []option.RequestOption{option.WithHTTPClient(cfg.HTTPClient)}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	// Fallbacks are a Claude API feature; a proxy or another platform may
	// refuse the field.
	direct := cfg.BaseURL == "" && os.Getenv("ANTHROPIC_BASE_URL") == ""
	return &anthropicModel{client: anthropic.NewClient(opts...), model: cfg.Model, fallbacks: direct && fallbackModels[cfg.Model]}
}

func (m *anthropicModel) complete(ctx context.Context, system, text string, schema map[string]any) (string, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(m.model),
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text))},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Format: anthropic.BetaJSONOutputFormatParam{Schema: schema},
		},
	}
	if m.fallbacks {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	msg, err := m.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("model: anthropic: %w", err)
	}
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return "", fmt.Errorf("model: anthropic: %s declined to extract from this text", m.model)
	case anthropic.BetaStopReasonMaxTokens:
		return "", fmt.Errorf("model: anthropic: reply was cut off at max_tokens; remember a shorter text")
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String(), nil
}

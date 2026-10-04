package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// chatCompletions calls any endpoint that speaks the OpenAI chat completions
// API: OpenAI itself, Ollama, vLLM, Groq, and others.
type chatCompletions struct {
	base, key, model string
	http             *http.Client
}

func (c *chatCompletions) complete(ctx context.Context, system, text string, schema map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": text},
		},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "statements", "strict": true, "schema": schema},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("model: %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("model: %s: %w", c.base, err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if resp.StatusCode/100 != 2 {
		if json.Unmarshal(raw, &out) == nil && out.Error != nil {
			return "", fmt.Errorf("model: %s: %s: %s", c.base, resp.Status, out.Error.Message)
		}
		return "", fmt.Errorf("model: %s: %s: %s", c.base, resp.Status, strings.TrimSpace(string(raw[:min(len(raw), 500)])))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("model: %s: malformed reply: %w", c.base, err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("model: %s: reply has no choices", c.base)
	}
	choice := out.Choices[0]
	switch {
	case choice.Message.Refusal != "":
		return "", fmt.Errorf("model: %s declined to extract from this text: %s", c.model, choice.Message.Refusal)
	case choice.FinishReason == "length":
		return "", fmt.Errorf("model: %s: reply was cut off; remember a shorter text", c.model)
	}
	return choice.Message.Content, nil
}

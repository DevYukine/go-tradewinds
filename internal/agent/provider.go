package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// truncateBody limits a response body string to maxLen characters for error messages.
func truncateBody(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...(truncated)"
}

// LLMProvider abstracts an LLM backend so the agent can call different
// services (Anthropic, OpenAI, Ollama) through a uniform interface.
type LLMProvider interface {
	// Complete sends a system prompt and user prompt to the LLM and returns
	// the text response. Implementations must respect ctx for cancellation.
	Complete(ctx context.Context, systemPrompt string, userPrompt string) (string, error)
}

// ---------------------------------------------------------------------------
// Claude (Anthropic) provider
// ---------------------------------------------------------------------------

// ClaudeProvider calls the Anthropic Messages API.
type ClaudeProvider struct {
	apiKey string
	model  string
	maxTok int
	client *http.Client
}

// NewClaudeProvider creates a provider targeting the Anthropic API.
func NewClaudeProvider(apiKey, model string, maxTokens int) *ClaudeProvider {
	return &ClaudeProvider{
		apiKey: apiKey,
		model:  model,
		maxTok: maxTokens,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *ClaudeProvider) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	body := map[string]any{
		"model":      p.model,
		"max_tokens": p.maxTok,
		"system":     systemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("claude: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("claude: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("claude: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("claude: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("claude: HTTP %d: %s", resp.StatusCode, truncateBody(string(respBody), 500))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("claude: unmarshal response: %w", err)
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("claude: empty content in response")
	}
	content := result.Content[0].Text
	if content == "" {
		return "", fmt.Errorf("claude: empty text in response: %s", truncateBody(string(respBody), 500))
	}
	return content, nil
}

// ---------------------------------------------------------------------------
// OpenAI provider
// ---------------------------------------------------------------------------

// OpenAIProvider calls the OpenAI Chat Completions API.
type OpenAIProvider struct {
	apiKey string
	model  string
	maxTok int
	client *http.Client
}

// NewOpenAIProvider creates a provider targeting the OpenAI API.
func NewOpenAIProvider(apiKey, model string, maxTokens int) *OpenAIProvider {
	return &OpenAIProvider{
		apiKey: apiKey,
		model:  model,
		maxTok: maxTokens,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *OpenAIProvider) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	body := map[string]any{
		"model":      p.model,
		"max_tokens": p.maxTok,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("openai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.openai.com/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("openai: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("openai: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, truncateBody(string(respBody), 500))
	}

	var result struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("openai: unmarshal response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices in response")
	}
	content := result.Choices[0].Message.Content
	if content == "" {
		return "", fmt.Errorf("openai: empty content (model=%s, finish_reason=%s)", result.Model, result.Choices[0].FinishReason)
	}
	return content, nil
}

// ---------------------------------------------------------------------------
// OpenRouter provider
// ---------------------------------------------------------------------------

// OpenRouterProvider calls the OpenRouter API, which is OpenAI-compatible
// but routes to many different models (Claude, GPT, Llama, Gemini, etc.).
type OpenRouterProvider struct {
	apiKey string
	model  string
	maxTok int
	client *http.Client
}

// NewOpenRouterProvider creates a provider targeting the OpenRouter API.
func NewOpenRouterProvider(apiKey, model string, maxTokens int) *OpenRouterProvider {
	return &OpenRouterProvider{
		apiKey: apiKey,
		model:  model,
		maxTok: maxTokens,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *OpenRouterProvider) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	body := map[string]any{
		"model":      p.model,
		"max_tokens": p.maxTok,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		// Disable extended thinking/reasoning for models that support it.
		// Reasoning models burn tokens on internal CoT, leaving none for content.
		"reasoning": map[string]any{"effort": "none"},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("openrouter: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("openrouter: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("openrouter: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openrouter: HTTP %d: %s", resp.StatusCode, truncateBody(string(respBody), 500))
	}

	var result struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Model    string `json:"model"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("openrouter: unmarshal response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("openrouter: no choices in response")
	}
	content := result.Choices[0].Message.Content
	if content == "" {
		return "", fmt.Errorf("openrouter: empty content (model=%s, provider=%s, finish_reason=%s)", result.Model, result.Provider, result.Choices[0].FinishReason)
	}
	return content, nil
}

// ---------------------------------------------------------------------------
// Ollama provider (local)
// ---------------------------------------------------------------------------

// OllamaProvider calls a local Ollama instance.
type OllamaProvider struct {
	model  string
	url    string
	client *http.Client
}

// NewOllamaProvider creates a provider targeting a local Ollama server.
func NewOllamaProvider(model string) *OllamaProvider {
	return &OllamaProvider{
		model:  model,
		url:    "http://localhost:11434/api/generate",
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *OllamaProvider) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	body := map[string]any{
		"model":  p.model,
		"system": systemPrompt,
		"prompt": userPrompt,
		"stream": false,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.url, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("ollama: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ollama: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, truncateBody(string(respBody), 500))
	}

	var result struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("ollama: unmarshal response: %w", err)
	}
	return result.Response, nil
}

package llm

import (
	"context"
	"fmt"

	"github.com/Nahua-Foundation/ragota/internal/httpx"
)

// Generator produces text completions (used for code summaries).
type Generator interface {
	// Name returns the generator name.
	Name() string
	// Generate returns a completion for the prompt.
	Generate(ctx context.Context, prompt string) (string, error)
}

// --- Ollama ---

// OllamaGenerator implements Generator with the Ollama /api/generate endpoint.
type OllamaGenerator struct {
	client *httpx.Client
	model  string
}

// NewOllamaGenerator creates an Ollama text generator.
func NewOllamaGenerator(baseURL, model string) (*OllamaGenerator, error) {
	if model == "" {
		return nil, fmt.Errorf("ollama generator: model is required")
	}
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return &OllamaGenerator{
		client: &httpx.Client{BaseURL: baseURL},
		model:  model,
	}, nil
}

// Name returns the generator name.
func (g *OllamaGenerator) Name() string { return "ollama" }

type ollamaGenReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaGenResp struct {
	Response string `json:"response"`
}

// Generate returns a completion for the prompt.
func (g *OllamaGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	var resp ollamaGenResp
	if err := g.client.PostJSON(ctx, "/api/generate", ollamaGenReq{Model: g.model, Prompt: prompt, Stream: false}, &resp); err != nil {
		return "", err
	}
	return resp.Response, nil
}

// --- OpenAI ---

// OpenAIGenerator implements Generator with the OpenAI chat completions API.
type OpenAIGenerator struct {
	client *httpx.Client
	model  string
}

// NewOpenAIGenerator creates an OpenAI-compatible chat generator.
func NewOpenAIGenerator(baseURL, apiKey, model string) (*OpenAIGenerator, error) {
	if model == "" {
		return nil, fmt.Errorf("openai generator: model is required")
	}
	base := normalizeOpenAIBase(baseURL)
	// Self-hosted OpenAI-compatible gateways (vLLM, LiteLLM) usually need no
	// credential; only the public endpoint always does.
	if apiKey == "" && base == defaultOpenAIBase {
		return nil, fmt.Errorf("openai generator: api key is required")
	}
	return &OpenAIGenerator{
		client: &httpx.Client{
			BaseURL: base,
			Header:  authHeader(apiKey),
		},
		model: model,
	}, nil
}

// Name returns the generator name.
func (g *OpenAIGenerator) Name() string { return "openai" }

type openaiChatReq struct {
	Model    string          `json:"model"`
	Messages []openaiChatMsg `json:"messages"`
}

type openaiChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiChatResp struct {
	Choices []struct {
		Message openaiChatMsg `json:"message"`
	} `json:"choices"`
}

// Generate returns a completion for the prompt.
func (g *OpenAIGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	req := openaiChatReq{
		Model:    g.model,
		Messages: []openaiChatMsg{{Role: "user", Content: prompt}},
	}
	var resp openaiChatResp
	if err := g.client.PostJSON(ctx, "/v1/chat/completions", req, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("openai generator: empty response")
	}
	return resp.Choices[0].Message.Content, nil
}

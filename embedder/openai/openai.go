// Package openai is an openagent built-in embedding backend: an OpenAI-
// compatible /embeddings API (same wire format served by OpenAI, Ollama,
// Jina, Cohere, and local gateway proxies). Configure it in settings
// instead of the built-in BGE embedder to use a hosted embedding model.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// OpenAI embeds text via an OpenAI-compatible /embeddings endpoint.
type OpenAI struct {
	baseURL   string
	apiKey    string
	model     string
	client    *http.Client
	mu        sync.Mutex
	dimension int // cached from the first response
}

// New creates an embedder for baseURL+/embeddings.
func New(baseURL, apiKey, model string) *OpenAI {
	return &OpenAI{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

// Embed implements openagent.Embedder.
func (e *OpenAI) Embed(ctx context.Context, text string) ([]float64, error) {
	body, err := json.Marshal(map[string]any{
		"model": e.model,
		"input": text,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding: %s", resp.Status)
	}

	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embedding: decode: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding: empty response")
	}
	vec := out.Data[0].Embedding
	e.mu.Lock()
	if e.dimension == 0 {
		e.dimension = len(vec)
	}
	e.mu.Unlock()
	return vec, nil
}

// Dimensions returns the embedding dimension (cached from the first
// Embed call; 0 before any call).
func (e *OpenAI) Dimensions() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dimension
}

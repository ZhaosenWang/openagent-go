// Package openviking implements the OpenViking Context Database adapters:
// memory, skill, and resource providers backed by an OpenViking server.
//
// OpenViking is a Provider — the runtime never depends on it directly.
// The client talks to the OpenViking HTTP API directly (no SDK): the
// server's REST surface is the stable contract, and the provider needs
// only a handful of endpoints.
//
//	Search     — POST /api/v1/search/search  (semantic retrieval)
//	ListSkills — GET  /api/v1/skills         (list skill catalog)
//	Remember   — POST /api/v1/sessions → /messages/batch → /commit
//	Read       — GET  /api/v1/content/read  (viking:// URI → content)
package openviking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Item is a stored knowledge entry in OpenViking (one search hit).
type Item struct {
	ID      string         `json:"id,omitempty"`   // viking:// URI
	Kind    string         `json:"kind,omitempty"` // memory | resource | skill
	Content string         `json:"content"`        // abstract
	Meta    map[string]any `json:"meta,omitempty"`
	Score   float64        `json:"score,omitempty"`
}

// Client talks to an OpenViking server over its HTTP API. Knowledge is
// scoped by the server's own identity (API key / account / user), not by
// this client.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient creates a client for an OpenViking server.
func NewClient(endpoint string) (*Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("openviking: empty endpoint")
	}
	return &Client{
		baseURL: endpoint,
		http:    &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// responseEnvelope is the OpenViking API envelope: {status, result, error}.
type responseEnvelope struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// apiError is a non-2xx / envelope-error response.
type apiError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("openviking api: %s (%s, HTTP %d)", e.Message, e.Code, e.StatusCode)
	}
	return fmt.Sprintf("openviking api: HTTP %d: %s", e.StatusCode, e.Message)
}

// doJSON performs one API call and decodes the envelope's result into out.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openviking request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var env responseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return &apiError{StatusCode: resp.StatusCode, Message: truncate(string(data), 200)}
	}
	if env.Error != nil {
		return &apiError{StatusCode: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	if env.Status != "" && env.Status != "ok" {
		return &apiError{StatusCode: resp.StatusCode, Code: env.Status, Message: truncate(string(data), 200)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{StatusCode: resp.StatusCode, Message: truncate(string(data), 200)}
	}
	if out == nil || len(env.Result) == 0 || string(env.Result) == "null" {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// ── Search ──

// findResult mirrors the /api/v1/search/search response.
type findResult struct {
	Memories  []matchedContext `json:"memories,omitempty"`
	Resources []matchedContext `json:"resources,omitempty"`
	Skills    []matchedContext `json:"skills,omitempty"`
}

// matchedContext is one retrieval hit.
type matchedContext struct {
	URI      string  `json:"uri,omitempty"`
	Abstract string  `json:"abstract,omitempty"`
	Score    float64 `json:"score,omitempty"`
}

// Search runs the server's semantic retrieval. contextType narrows to one
// index ("memory" | "resource" | "skill"); empty returns everything.
func (c *Client) Search(ctx context.Context, query string, limit int, contextType ...string) ([]Item, error) {
	payload := map[string]any{
		"query": query,
		"limit": limit,
	}
	kind := "memory"
	if len(contextType) > 0 && contextType[0] != "" {
		kind = contextType[0]
		payload["context_type"] = kind
	}

	var res findResult
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/search/search", nil, payload, &res); err != nil {
		return nil, fmt.Errorf("openviking search: %w", err)
	}

	var hits []matchedContext
	switch kind {
	case "resource":
		hits = res.Resources
	case "skill":
		hits = res.Skills
	default:
		hits = res.Memories
	}
	items := make([]Item, 0, len(hits))
	for _, m := range hits {
		items = append(items, Item{
			Kind:    kind,
			ID:      m.URI,
			Content: m.Abstract,
			Score:   m.Score,
		})
	}
	return items, nil
}

// ── ListSkills ──

// SkillEntry is one skill in the GET /api/v1/skills catalog listing.
type SkillEntry struct {
	Name        string   `json:"name"`
	URI         string   `json:"uri"`
	RootURI     string   `json:"root_uri,omitempty"`
	SkillMDURI  string   `json:"skill_md_uri,omitempty"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Level       int      `json:"level,omitempty"`
}

// listSkillsResult mirrors the GET /api/v1/skills response result.
type listSkillsResult struct {
	Skills []SkillEntry `json:"skills,omitempty"`
	Total  int          `json:"total,omitempty"`
}

// ListSkills lists the full skill catalog from OpenViking. nodeLimit caps
// the number of nodes returned per skill (0 = server default). Unlike
// Search, this endpoint requires no query and returns every skill.
func (c *Client) ListSkills(ctx context.Context, nodeLimit int) ([]SkillEntry, error) {
	query := url.Values{}
	if nodeLimit > 0 {
		query.Set("node_limit", fmt.Sprintf("%d", nodeLimit))
	}
	var res listSkillsResult
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/skills", query, nil, &res); err != nil {
		return nil, fmt.Errorf("openviking list skills: %w", err)
	}
	return res.Skills, nil
}

// ── Remember ──

// Remember stores a message into OpenViking long-term memory (triggers
// the server's background extraction).
func (c *Client) Remember(ctx context.Context, content string) (string, error) {
	// 1. Create a session.
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/sessions", nil, map[string]any{}, &created); err != nil {
		return "", fmt.Errorf("openviking create session: %w", err)
	}
	if created.SessionID == "" {
		return "", fmt.Errorf("openviking create session: empty session id")
	}

	// 2. Add the message.
	msg := map[string]any{"role": "user", "content": content}
	if err := c.doJSON(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(created.SessionID)+"/messages/batch",
		nil, map[string]any{"messages": []any{msg}}, nil); err != nil {
		return "", fmt.Errorf("openviking add messages: %w", err)
	}

	// 3. Commit (archive + background extraction).
	if err := c.doJSON(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(created.SessionID)+"/commit",
		nil, map[string]any{"keep_recent_count": 0}, nil); err != nil {
		return "", fmt.Errorf("openviking commit session: %w", err)
	}
	return "stored", nil
}

// ── Read ──

// Read expands a viking:// URI to its full content.
func (c *Client) Read(ctx context.Context, uri string) (string, error) {
	query := url.Values{"uri": []string{uri}}
	var content string
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/content/read", query, nil, &content); err != nil {
		return "", fmt.Errorf("openviking read %s: %w", uri, err)
	}
	return content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

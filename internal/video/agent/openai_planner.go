package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// OpenAIPlanner adapts any OpenAI-compatible chat-completions endpoint to
// the Planner interface. In production it is normally pointed at the
// gateway's own data plane (loopback), so the planning call reuses the
// operator's existing provider configuration, routing, failover, budgets,
// and credentials — the video engine never needs a second copy of an
// upstream API key. The API key, when one is required, is read from the
// environment variable named by APIKeyEnv and is never logged or returned.
type OpenAIPlanner struct {
	BaseURL     string // e.g. http://127.0.0.1:8080 (gateway) or an upstream base
	APIKeyEnv   string // environment variable holding the bearer token, if any
	JSONMode    bool   // send response_format {"type":"json_object"}
	Timeout     time.Duration
	MaxTokens   int
	Temperature float64
	Client      *http.Client
}

// NewOpenAIPlanner validates the endpoint. Plain HTTP is accepted only for
// loopback addresses, mirroring the ComfyUI adapter's rule; remote planner
// endpoints must use HTTPS.
func NewOpenAIPlanner(baseURL, apiKeyEnv string, jsonMode bool) (*OpenAIPlanner, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, errors.New("planner base URL is required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("planner base URL %q is not a valid http(s) URL", baseURL)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("planner base URL uses plain HTTP for a non-loopback host; use HTTPS")
	}
	if strings.TrimSpace(apiKeyEnv) != "" && strings.TrimSpace(os.Getenv(apiKeyEnv)) == "" {
		return nil, fmt.Errorf("planner API key environment variable %q is missing or empty", apiKeyEnv)
	}
	return &OpenAIPlanner{BaseURL: base, APIKeyEnv: strings.TrimSpace(apiKeyEnv), JSONMode: jsonMode, Timeout: 120 * time.Second, MaxTokens: 4096}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Stream         bool            `json:"stream"`
}
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type responseFormat struct {
	Type string `json:"type"`
}
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Complete performs one non-streaming chat completion and returns the
// assistant's text. Errors never include the API key or response secrets;
// upstream error bodies are truncated and treated as untrusted text.
func (p *OpenAIPlanner) Complete(ctx context.Context, model, system, user string) (string, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload := chatRequest{
		Model:       model,
		Messages:    []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: p.Temperature,
		MaxTokens:   p.MaxTokens,
		Stream:      false,
	}
	if p.JSONMode {
		payload.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.APIKeyEnv != "" {
		key := strings.TrimSpace(os.Getenv(p.APIKeyEnv))
		if key == "" {
			return "", fmt.Errorf("planner API key environment variable %q is missing or empty", p.APIKeyEnv)
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: timeout + 10*time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("planner request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read planner response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(data))
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return "", fmt.Errorf("planner endpoint returned HTTP %d: %s", resp.StatusCode, snippet)
	}
	var decoded chatResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return "", fmt.Errorf("planner response is not valid chat-completions JSON: %w", err)
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return "", errors.New("planner response contains no message content")
	}
	return decoded.Choices[0].Message.Content, nil
}

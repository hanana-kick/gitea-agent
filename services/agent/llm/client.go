// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"` // system, user, assistant
	Content string `json:"content"`
}

// ChatRequest is the request body for chat completion.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
}

// ChatResponse is the response body for chat completion.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
}

// Choice represents a single completion choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ModelArchitecture describes how a model takes and returns data. Only some
// providers publish it; the field stays empty elsewhere.
type ModelArchitecture struct {
	InputModalities []string `json:"input_modalities"`
}

// ModelInfo is a model offered by a provider.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Architecture is nil when the provider does not describe its models.
	Architecture *ModelArchitecture `json:"architecture,omitempty"`
}

// InputModalities returns the inputs the model accepts, or nil when unknown.
func (m ModelInfo) InputModalities() []string {
	if m.Architecture == nil {
		return nil
	}
	modalities := make([]string, 0, len(m.Architecture.InputModalities))
	for _, modality := range m.Architecture.InputModalities {
		if modality = strings.TrimSpace(modality); modality != "" {
			modalities = append(modalities, modality)
		}
	}
	slices.Sort(modalities)
	return modalities
}

// Client is the interface for LLM providers.
type Client interface {
	// Chat sends a chat completion request and returns the assistant's reply.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// ListModels returns the models the provider exposes.
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// DoListModels fetches and parses a model listing endpoint. Both the OpenAI
// envelope ({"data": [...]}) and a bare array are accepted.
func DoListModels(ctx context.Context, httpClient HTTPClient, url string, header http.Header) ([]ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		httpReq.Header[k] = v
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("LLM API error: " + string(respBody))
	}

	var envelope struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(respBody, &envelope); err == nil && envelope.Data != nil {
		return cleanModels(envelope.Data), nil
	}
	var bare []ModelInfo
	if err := json.Unmarshal(respBody, &bare); err != nil {
		return nil, err
	}
	return cleanModels(bare), nil
}

func cleanModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	seen := map[string]bool{}
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelInfo{ID: id, Name: strings.TrimSpace(m.Name), Architecture: m.Architecture})
	}
	return out
}

// HTTPClient is an interface for making HTTP requests (wraps *http.Client).
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// normalizeBaseURL returns baseURL without a trailing slash, or def when baseURL is empty.
func normalizeBaseURL(baseURL, def string) string {
	if baseURL == "" {
		return def
	}
	return strings.TrimSuffix(baseURL, "/")
}

// headersFromMap converts custom headers into an http.Header, skipping empty names.
func headersFromMap(headers map[string]string) http.Header {
	h := http.Header{}
	for k, v := range headers {
		if k == "" {
			continue
		}
		h.Set(k, v)
	}
	return h
}

// DoChat sends a chat request to the given endpoint with the given headers and returns the parsed response.
func DoChat(ctx context.Context, httpClient HTTPClient, method, url string, header http.Header, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		httpReq.Header[k] = v
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, errors.New("LLM API error: " + string(respBody))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return ChatResponse{}, err
	}
	return chatResp, nil
}

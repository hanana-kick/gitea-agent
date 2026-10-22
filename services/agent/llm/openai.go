// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"maps"
	"net/http"
)

// OpenAIClient talks to any OpenAI-compatible chat completions endpoint
// (OpenAI, OpenRouter, NVIDIA NIM, or a custom compatible gateway).
type OpenAIClient struct {
	HTTPClient   HTTPClient
	ProviderType string
	APIKey       string
	BaseURL      string
	ModelsURL    string
	Model        string
	ExtraHeaders http.Header
}

// NewOpenAIClient creates a new OpenAI-compatible client.
// baseURL defaults to https://api.openai.com/v1 if empty.
func NewOpenAIClient(providerType, apiKey, baseURL, modelsURL, model string, extraHeaders map[string]string) *OpenAIClient {
	baseURL = normalizeBaseURL(baseURL, "https://api.openai.com/v1")
	if modelsURL == "" {
		modelsURL = baseURL + "/models"
	}
	return &OpenAIClient{
		HTTPClient:   &http.Client{},
		ProviderType: providerType,
		APIKey:       apiKey,
		BaseURL:      baseURL,
		ModelsURL:    modelsURL,
		Model:        model,
		ExtraHeaders: headersFromMap(extraHeaders),
	}
}

// authHeader returns the headers used for every request to this provider.
func (c *OpenAIClient) authHeader() http.Header {
	header := http.Header{
		"Authorization": {"Bearer " + c.APIKey},
	}
	maps.Copy(header, c.ExtraHeaders)
	return header
}

// ListModels returns the models the endpoint exposes.
func (c *OpenAIClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	return DoListModels(ctx, c.HTTPClient, c.ModelsURL, c.authHeader())
}

// Chat sends a chat completion request to an OpenAI-compatible endpoint.
func (c *OpenAIClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.Model
	}
	return DoChat(ctx, c.HTTPClient, http.MethodPost, c.BaseURL+"/chat/completions", c.authHeader(), req)
}

// ChatStream sends a chat completion request and reports the answer as it arrives.
func (c *OpenAIClient) ChatStream(ctx context.Context, req ChatRequest, onDelta OnDelta) (ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.Model
	}
	return DoChatStream(ctx, c.HTTPClient, c.BaseURL+"/chat/completions", c.authHeader(), req, onDelta)
}

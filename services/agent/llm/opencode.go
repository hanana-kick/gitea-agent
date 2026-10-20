// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"net/http"
)

// SessionHeader is the header OpenCode uses to carry its session credential.
const SessionHeader = "X-Opencode-Session"

// OpenCodeClient is a client for the OpenCode inference gateway, shared by the
// Zen (pay-as-you-go) and Go (subscription) plans.
type OpenCodeClient struct {
	HTTPClient   HTTPClient
	APIKey       string
	SessionID    string // empty for plans that do not require the session header
	BaseURL      string
	ModelsURL    string
	Model        string
	ExtraHeaders http.Header
}

// NewOpenCodeClient creates a new OpenCode API client.
// sessionID is sent as the fixed session header; baseURL defaults to
// https://api.opencode.ai/v1 if empty.
func NewOpenCodeClient(apiKey, sessionID, baseURL, modelsURL, model string, extraHeaders map[string]string) *OpenCodeClient {
	baseURL = normalizeBaseURL(baseURL, "https://opencode.ai/inference/openai/v1")
	if modelsURL == "" {
		modelsURL = baseURL + "/models"
	}
	return &OpenCodeClient{
		HTTPClient:   &http.Client{},
		APIKey:       apiKey,
		SessionID:    sessionID,
		BaseURL:      baseURL,
		ModelsURL:    modelsURL,
		Model:        model,
		ExtraHeaders: headersFromMap(extraHeaders),
	}
}

// authHeader returns the headers used for every request: the bearer credential,
// then custom headers, then the fixed session header, which custom headers may not replace.
func (c *OpenCodeClient) authHeader() http.Header {
	header := http.Header{"Authorization": {"Bearer " + c.APIKey}}
	for k, v := range c.ExtraHeaders {
		header[k] = v
	}
	if c.SessionID != "" {
		header.Set(SessionHeader, c.SessionID)
	}
	return header
}

// ListModels returns the models the OpenCode gateway exposes.
func (c *OpenCodeClient) ListModels(ctx context.Context) ([]ModelInfo, error) {
	return DoListModels(ctx, c.HTTPClient, c.ModelsURL, c.authHeader())
}

// Chat sends a chat completion request to the OpenCode API.
func (c *OpenCodeClient) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if req.Model == "" {
		req.Model = c.Model
	}
	return DoChat(ctx, c.HTTPClient, http.MethodPost, c.BaseURL+"/chat/completions", c.authHeader(), req)
}

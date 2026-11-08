// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseURLOf(t *testing.T, client Client) string {
	t.Helper()
	switch c := client.(type) {
	case *OpenAIClient:
		return c.BaseURL
	case *OpenCodeClient:
		return c.BaseURL
	default:
		t.Fatalf("unknown client type %T", client)
		return ""
	}
}

func TestNewClientDefaultsBaseURL(t *testing.T) {
	for _, info := range Providers() {
		t.Run(info.Type, func(t *testing.T) {
			client, err := NewClient(info.Type, "key", "", "model", nil)
			require.NoError(t, err)
			assert.Equal(t, info.DefaultBaseURL, baseURLOf(t, client))
		})
	}
}

func TestResolveBaseURL(t *testing.T) {
	assert.Equal(t, "https://api.openai.com/v1", ResolveBaseURL(ProviderOpenAI, ""))
	assert.Equal(t, "https://example.com/v1", ResolveBaseURL(ProviderOpenAI, "https://example.com/v1/"))
	assert.Equal(t, "https://openrouter.ai/api/v1", ResolveBaseURL(ProviderOpenRouter, ""))
	assert.Equal(t, "https://opencode.ai/inference/openai/v1", ResolveBaseURL(ProviderOpenCodeZen, ""))
	assert.Equal(t, "https://opencode.ai/inference/go/openai/v1", ResolveBaseURL(ProviderOpenCodeGo, ""))
}

func TestNewClientUnsupportedProvider(t *testing.T) {
	_, err := NewClient("unknown", "key", "", "model", nil)
	assert.Error(t, err)
	assert.False(t, IsSupportedProvider("unknown"))
	assert.Len(t, Providers(), 5)
}

// chatServer records the headers of the last chat request it received.
func chatServer(t *testing.T, seen *http.Header) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(ChatResponse{
			Model:   "test-model",
			Choices: []Choice{{Message: Message{Role: "assistant", Content: "pong"}}},
		}))
	}))
}

func TestOpenAIClientSendsCustomHeaders(t *testing.T) {
	var seen http.Header
	server := chatServer(t, &seen)
	defer server.Close()

	client, err := NewClient(ProviderOpenAI, "secret-key", server.URL, "gpt-4o", map[string]string{
		"X-Custom-Header": "custom-value",
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), ChatRequest{})
	require.NoError(t, err)

	assert.Equal(t, "Bearer secret-key", seen.Get("Authorization"))
	assert.Equal(t, "custom-value", seen.Get("X-Custom-Header"))
}

func TestOpenAIClientCustomHeaderOverridesAuthorization(t *testing.T) {
	var seen http.Header
	server := chatServer(t, &seen)
	defer server.Close()

	client, err := NewClient(ProviderOpenAI, "secret-key", server.URL, "gpt-4o", map[string]string{
		"Authorization": "Api-Key azure-style",
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), ChatRequest{})
	require.NoError(t, err)

	assert.Equal(t, "Api-Key azure-style", seen.Get("Authorization"))
}

func TestOpenCodeGoSendsCredentialAndSessionHeader(t *testing.T) {
	var seen http.Header
	server := chatServer(t, &seen)
	defer server.Close()

	// Go needs both the credential and a session id; custom headers may not replace the session.
	client, err := NewClient(ProviderOpenCodeGo, "sk-opencode", server.URL, "opencode-model", map[string]string{
		SessionHeader:     "hijacked",
		"X-Custom-Header": "custom-value",
	})
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), ChatRequest{})
	require.NoError(t, err)

	assert.Equal(t, "Bearer sk-opencode", seen.Get("Authorization"))
	assert.Equal(t, "sk-opencode", seen.Get(SessionHeader))
	assert.Equal(t, "custom-value", seen.Get("X-Custom-Header"))
}

func TestOpenCodeZenSendsNoSessionHeader(t *testing.T) {
	var seen http.Header
	server := chatServer(t, &seen)
	defer server.Close()

	// the pay-as-you-go plan authenticates with the credential only
	client, err := NewClient(ProviderOpenCodeZen, "sk-zen", server.URL, "opencode-model", nil)
	require.NoError(t, err)

	_, err = client.Chat(context.Background(), ChatRequest{})
	require.NoError(t, err)

	assert.Equal(t, "Bearer sk-zen", seen.Get("Authorization"))
	assert.Empty(t, seen.Get(SessionHeader))
}

func TestProviderEndpoints(t *testing.T) {
	zen, ok := Provider(ProviderOpenCodeZen)
	require.True(t, ok)
	assert.Equal(t, "https://opencode.ai/inference/openai/v1", zen.DefaultBaseURL)
	assert.Equal(t, "https://opencode.ai/inference/v1/models", zen.ModelsURL)
	assert.Empty(t, zen.FixedHeaders)

	goPlan, ok := Provider(ProviderOpenCodeGo)
	require.True(t, ok)
	assert.Equal(t, "https://opencode.ai/inference/go/openai/v1", goPlan.DefaultBaseURL)
	assert.Equal(t, "https://opencode.ai/inference/go/v1/models", goPlan.ModelsURL)
	assert.Equal(t, []string{SessionHeader}, goPlan.FixedHeaders)

	// only the OpenAI-compatible slot makes the operator supply the endpoint
	for _, info := range Providers() {
		assert.Equal(t, info.Type == ProviderOpenAI, info.RequiresBaseURL, info.Type)
	}
}

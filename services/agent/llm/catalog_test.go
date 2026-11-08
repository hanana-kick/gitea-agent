// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogReadsOnlyWantedProviders(t *testing.T) {
	const payload = `{
	  "openai": {"models": {"gpt-4o": {"modalities": {"input": ["text", "image"], "output": ["text"]}, "limit": {"context": 128000, "output": 16384}}}},
	  "unrelated": {"models": {"x": {"modalities": {"input": ["text"]}}}},
	  "opencode": {"models": {"glm-5.3": {"modalities": {"input": ["text"]}}}}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()

	originalURL := ModelCatalogURL
	ModelCatalogURL = server.URL
	t.Cleanup(func() { ModelCatalogURL = originalURL })
	resetCatalogCache()

	catalog := LoadCatalog(context.Background())
	require.NotNil(t, catalog)

	assert.Equal(t, []string{"image", "text"}, catalog.InputModalities(ProviderOpenAI, "gpt-4o"))
	assert.Equal(t, []string{"text"}, catalog.InputModalities(ProviderOpenCodeGo, "glm-5.3"))
	// a provider the catalog does not cover, and an unknown model, yield nothing
	assert.Nil(t, catalog.InputModalities(ProviderOpenAI, "unknown-model"))
	assert.Nil(t, catalog.InputModalities("unrelated-provider", "x"))

	// the output limit a model advertises is what a request carries
	assert.Equal(t, 16384, catalog.OutputLimit(ProviderOpenAI, "gpt-4o"))
	assert.Equal(t, 0, catalog.OutputLimit(ProviderOpenAI, "unknown-model"), "an unknown model has no limit")
	assert.Equal(t, 0, catalog.OutputLimit(ProviderOpenCodeGo, "glm-5.3"), "a model without a limit has none")
	assert.Equal(t, 0, MaxOutputTokens(context.Background(), ProviderOpenAI, "unknown-model"))
}

func TestCatalogSurvivesFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	originalURL := ModelCatalogURL
	ModelCatalogURL = server.URL
	t.Cleanup(func() { ModelCatalogURL = originalURL })
	resetCatalogCache()

	// an unreachable catalog must not break a model list
	assert.Nil(t, LoadCatalog(context.Background()))
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import "fmt"

// Supported provider type identifiers, as stored in agent_provider.provider.
const (
	ProviderOpenAI      = "openai"
	ProviderOpenRouter  = "openrouter"
	ProviderNVIDIA      = "nvidia"
	ProviderOpenCodeZen = "opencode-zen"
	ProviderOpenCodeGo  = "opencode-go"
)

// ProviderInfo describes a provider type: where it points by default and which
// headers it always sends. The order of Providers is the order shown in UIs.
type ProviderInfo struct {
	Type           string
	DefaultBaseURL string
	// RequiresBaseURL marks a provider that only works against a user-supplied
	// endpoint (the OpenAI-compatible slot covers self-hosted gateways).
	RequiresBaseURL bool
	// FixedHeaders are sent on every request and cannot be overridden by custom headers.
	FixedHeaders []string
	// ModelsURL is where the model list lives, when it differs from BaseURL + "/models".
	ModelsURL string
	// ModelsDevProvider is the identifier this provider goes by in the shared model
	// catalog, the fallback source of input modalities.
	ModelsDevProvider string
}

var providerInfos = []ProviderInfo{
	{
		Type:              ProviderOpenAI,
		DefaultBaseURL:    "https://api.openai.com/v1",
		RequiresBaseURL:   true,
		ModelsDevProvider: "openai",
	},
	{Type: ProviderOpenRouter, DefaultBaseURL: "https://openrouter.ai/api/v1", ModelsDevProvider: "openrouter"},
	{Type: ProviderNVIDIA, DefaultBaseURL: "https://integrate.api.nvidia.com/v1", ModelsDevProvider: "nvidia"},
	// OpenCode serves the OpenAI-compatible chat path and the model list from different roots;
	// Go additionally requires the session header.
	{
		Type:              ProviderOpenCodeZen,
		DefaultBaseURL:    "https://opencode.ai/inference/openai/v1",
		ModelsURL:         "https://opencode.ai/inference/v1/models",
		ModelsDevProvider: "opencode",
	},
	{
		Type:              ProviderOpenCodeGo,
		DefaultBaseURL:    "https://opencode.ai/inference/go/openai/v1",
		ModelsURL:         "https://opencode.ai/inference/go/v1/models",
		FixedHeaders:      []string{SessionHeader},
		ModelsDevProvider: "opencode",
	},
}

// Providers returns the supported provider types in display order.
func Providers() []ProviderInfo {
	return providerInfos
}

// Provider looks up a provider type.
func Provider(providerType string) (ProviderInfo, bool) {
	for _, info := range providerInfos {
		if info.Type == providerType {
			return info, true
		}
	}
	return ProviderInfo{}, false
}

// IsSupportedProvider reports whether providerType is a known provider.
func IsSupportedProvider(providerType string) bool {
	_, ok := Provider(providerType)
	return ok
}

// ResolveBaseURL returns the endpoint to use: the trimmed custom baseURL, or the
// provider's default when baseURL is empty.
func ResolveBaseURL(providerType, baseURL string) string {
	info, ok := Provider(providerType)
	if !ok {
		return normalizeBaseURL(baseURL, "")
	}
	return normalizeBaseURL(baseURL, info.DefaultBaseURL)
}

// NewClient creates a chat client for the given provider type.
// headers are optional extra request headers; they override the client's own
// authentication headers but never the provider's fixed headers.
func NewClient(providerType, apiKey, baseURL, model string, headers map[string]string) (Client, error) {
	info, ok := Provider(providerType)
	if !ok {
		return nil, fmt.Errorf("unsupported provider type: %s", providerType)
	}
	resolved := ResolveBaseURL(providerType, baseURL)
	modelsURL := resolved + "/models"
	// the published model list may live outside the chat root, but a custom endpoint is taken as-is
	if info.ModelsURL != "" && resolved == info.DefaultBaseURL {
		modelsURL = info.ModelsURL
	}

	switch providerType {
	case ProviderOpenCodeZen:
		return NewOpenCodeClient(apiKey, "", resolved, modelsURL, model, headers), nil
	case ProviderOpenCodeGo:
		// Go needs a stable session identifier in addition to the credential.
		return NewOpenCodeClient(apiKey, apiKey, resolved, modelsURL, model, headers), nil
	default:
		return NewOpenAIClient(providerType, apiKey, resolved, modelsURL, model, headers), nil
	}
}

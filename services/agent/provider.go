// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"slices"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	llm "gitea.dev/services/agent/llm"
)

// ErrUnknownModel is returned when a requested model is not configured on the provider.
type ErrUnknownModel struct {
	ID string
}

func (err ErrUnknownModel) Error() string {
	return "model not configured for this provider: " + err.ID
}

// Is reports whether target is an ErrUnknownModel.
func (err ErrUnknownModel) Is(target error) bool {
	_, ok := target.(ErrUnknownModel)
	return ok
}

// ErrNoModel is returned when a provider has no model to test.
type ErrNoModel struct{}

func (err ErrNoModel) Error() string {
	return "no model configured"
}

// Is reports whether target is an ErrNoModel.
func (err ErrNoModel) Is(target error) bool {
	_, ok := target.(ErrNoModel)
	return ok
}

// ErrNoAPIKey is returned when an operation needs a credential the provider does not have.
type ErrNoAPIKey struct{}

func (err ErrNoAPIKey) Error() string {
	return "no API key is bound to this provider"
}

// Is reports whether target is an ErrNoAPIKey.
func (err ErrNoAPIKey) Is(target error) bool {
	_, ok := target.(ErrNoAPIKey)
	return ok
}

// ErrUnsupportedProvider is returned when a provider type is not known.
type ErrUnsupportedProvider struct {
	Type string
}

func (err ErrUnsupportedProvider) Error() string {
	return "unsupported provider type: " + err.Type
}

// Is reports whether target is an ErrUnsupportedProvider regardless of the type.
func (err ErrUnsupportedProvider) Is(target error) bool {
	_, ok := target.(ErrUnsupportedProvider)
	return ok
}

// ProviderOptions holds the editable fields of a provider. APIKeyID names the credential in
// agent_api_key; 0 means the provider has none, which unbinds it on update.
type ProviderOptions struct {
	Name     string
	Provider string
	BaseURL  string
	Models   []agent_model.ProviderModel
	APIKeyID int64
	Headers  map[string]string
}

// ModelFetchOptions holds what is needed to list the models of a provider without storing it.
type ModelFetchOptions struct {
	Provider string
	BaseURL  string
	APIKeyID int64
	Headers  map[string]string
}

// FetchModels lists the models a provider offers, without storing anything. The
// catalog is passed through as the provider reports it: picking from it is the
// operator's job.
func FetchModels(ctx context.Context, opts ModelFetchOptions) ([]agent_model.ProviderModel, error) {
	apiKey, err := apiKeyValueOf(ctx, opts.APIKeyID)
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return nil, ErrNoAPIKey{}
	}

	client, err := llm.NewClient(opts.Provider, apiKey, opts.BaseURL, "", opts.Headers)
	if err != nil {
		return nil, err
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return nil, err
	}

	// providers rarely describe their models, so the shared catalog fills the gaps
	catalog := llm.LoadCatalog(ctx)
	out := make([]agent_model.ProviderModel, 0, len(models))
	for _, m := range models {
		inputs := m.InputModalities()
		if len(inputs) == 0 {
			inputs = catalog.InputModalities(opts.Provider, m.ID)
		}
		out = append(out, agent_model.ProviderModel{ID: m.ID, Name: m.Name, Inputs: inputs})
	}
	return agent_model.NormalizeModels(out), nil
}

// apiKeyValueOf decrypts a stored key by ID. A zero ID yields an empty value.
func apiKeyValueOf(ctx context.Context, id int64) (string, error) {
	if id == 0 {
		return "", nil
	}
	key, err := GetAPIKey(ctx, id)
	if err != nil {
		return "", err
	}
	return APIKeyValue(ctx, key), nil
}

// CreateProvider stores a new provider; userID only records who created it.
func CreateProvider(ctx context.Context, userID int64, opts ProviderOptions) (*agent_model.Provider, error) {
	if !llm.IsSupportedProvider(opts.Provider) {
		return nil, ErrUnsupportedProvider{Type: opts.Provider}
	}

	p := &agent_model.Provider{
		UserID:   userID,
		Name:     opts.Name,
		Provider: opts.Provider,
		BaseURL:  llm.ResolveBaseURL(opts.Provider, opts.BaseURL),
	}
	if err := p.SetModels(agent_model.NormalizeModels(opts.Models)); err != nil {
		return nil, fmt.Errorf("encode models: %w", err)
	}
	if opts.APIKeyID != 0 {
		if _, err := GetAPIKey(ctx, opts.APIKeyID); err != nil {
			return nil, err
		}
		p.APIKeyID = opts.APIKeyID
	}
	if err := p.SetCustomHeaders(opts.Headers); err != nil {
		return nil, fmt.Errorf("encode headers: %w", err)
	}

	if err := db.Insert(ctx, p); err != nil {
		return nil, fmt.Errorf("insert provider: %w", err)
	}
	return p, nil
}

// GetProviders returns every provider, optionally filtered by provider type.
func GetProviders(ctx context.Context, providerType string) ([]*agent_model.Provider, error) {
	providers, err := db.Find[agent_model.Provider](ctx, agent_model.FindProvidersOptions{
		Provider: providerType,
	})
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		p.HasKey = p.APIKeyID != 0
	}
	return providers, nil
}

// FindProviders returns one page of providers and the total number of matches.
func FindProviders(ctx context.Context, opts agent_model.FindProvidersOptions) ([]*agent_model.Provider, int64, error) {
	providers, total, err := db.FindAndCount[agent_model.Provider](ctx, opts)
	if err != nil {
		return nil, 0, err
	}
	for _, p := range providers {
		p.HasKey = p.APIKeyID != 0
	}
	return providers, total, nil
}

// GetProviderByID returns a provider by ID.
func GetProviderByID(ctx context.Context, id int64) (*agent_model.Provider, error) {
	p, exists, err := db.GetByID[agent_model.Provider](ctx, id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, agent_model.ErrProviderNotFound{ID: id}
	}
	p.HasKey = p.APIKeyID != 0
	return p, nil
}

// UpdateProvider updates provider fields and re-encrypts the API key if provided.
func UpdateProvider(ctx context.Context, id int64, opts ProviderOptions) (*agent_model.Provider, error) {
	if !llm.IsSupportedProvider(opts.Provider) {
		return nil, ErrUnsupportedProvider{Type: opts.Provider}
	}

	p, err := GetProviderByID(ctx, id)
	if err != nil {
		return nil, err
	}

	p.Name = opts.Name
	p.Provider = opts.Provider
	p.BaseURL = llm.ResolveBaseURL(opts.Provider, opts.BaseURL)
	if err := p.SetModels(agent_model.NormalizeModels(opts.Models)); err != nil {
		return nil, fmt.Errorf("encode models: %w", err)
	}
	cols := []string{"name", "provider", "base_url", "model", "models_json", "updated_unix", "api_key_id"}
	if opts.APIKeyID != 0 {
		if _, err := GetAPIKey(ctx, opts.APIKeyID); err != nil {
			return nil, err
		}
	}
	p.APIKeyID = opts.APIKeyID
	if err := p.SetCustomHeaders(opts.Headers); err != nil {
		return nil, fmt.Errorf("encode headers: %w", err)
	}
	cols = append(cols, "headers_json")

	_, err = db.GetEngine(ctx).ID(p.ID).Cols(cols...).Update(p)
	if err != nil {
		return nil, fmt.Errorf("update provider: %w", err)
	}
	return p, nil
}

// DeleteProvider removes a provider by ID.
func DeleteProvider(ctx context.Context, id int64) error {
	p, err := GetProviderByID(ctx, id)
	if err != nil {
		return err
	}
	if _, err := db.DeleteByID[agent_model.Provider](ctx, p.ID); err != nil {
		return fmt.Errorf("delete provider: %w", err)
	}
	return nil
}

// TestResult is the outcome of a provider connectivity test.
type TestResult struct {
	Model string // model that answered
	Reply string // assistant reply text
}

// TestProvider sends a minimal chat request to verify that the stored API key works.
// An empty model tests the provider's first configured model.
func TestProvider(ctx context.Context, id int64, model string) (*TestResult, error) {
	p, err := GetProviderByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if model == "" {
		model = p.Model
	} else if !slices.Contains(p.ModelIDs(), model) {
		return nil, ErrUnknownModel{ID: model}
	}
	if model == "" {
		return nil, ErrNoModel{}
	}

	apiKey := ResolveProviderAPIKey(ctx, p)
	if apiKey == "" {
		return nil, ErrNoAPIKey{}
	}

	client, err := llm.NewClient(p.Provider, apiKey, p.BaseURL, model, p.CustomHeaders())
	if err != nil {
		return nil, err
	}

	resp, err := client.Chat(ctx, llm.ChatRequest{
		Model:     model,
		Messages:  []llm.Message{{Role: "user", Content: "ping"}},
		MaxTokens: 5,
	})
	if err != nil {
		return nil, err
	}

	res := &TestResult{Model: model}
	if resp.Model != "" {
		res.Model = resp.Model
	}
	if len(resp.Choices) > 0 {
		res.Reply = resp.Choices[0].Message.Content
	}
	return res, nil
}

// GetDecryptedAPIKey returns the key a provider authenticates with, decrypted on demand.
func GetDecryptedAPIKey(ctx context.Context, id int64) (string, error) {
	p, err := GetProviderByID(ctx, id)
	if err != nil {
		return "", err
	}
	return ResolveProviderAPIKey(ctx, p), nil
}

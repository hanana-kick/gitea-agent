// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	actions_model "gitea.dev/models/actions"
	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	secret_model "gitea.dev/models/secret"
	"gitea.dev/modules/log"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
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

// ErrNoKeySource is returned when an update would leave the provider without a usable key.
type ErrNoKeySource struct{}

func (err ErrNoKeySource) Error() string {
	return "no API key source configured"
}

// Is reports whether target is an ErrNoKeySource.
func (err ErrNoKeySource) Is(target error) bool {
	_, ok := target.(ErrNoKeySource)
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

// Key sources a provider can use.
const (
	// KeySourceAPIKey stores the key encrypted in this instance.
	KeySourceAPIKey = "api_key"
	// KeySourceEnvKey reads the key from an Actions secret/variable or the process environment.
	KeySourceEnvKey = "env_key"
	// KeySourceKeep leaves the configured key untouched; only meaningful on update.
	KeySourceKeep = "keep"
)

// ProviderOptions holds the editable fields of a provider.
// On update an empty APIKey keeps the stored key, and KeySource "" keeps the whole key configuration.
type ProviderOptions struct {
	Name      string
	Provider  string
	BaseURL   string
	Models    []agent_model.ProviderModel
	KeySource string
	APIKey    string
	Headers   map[string]string
	EnvKey    string // instance-level Actions variable or environment variable name
}

// resolveKeySource infers the source for API clients that send only one of the
// two fields. An update that mentions neither leaves the configuration alone.
func (opts ProviderOptions) resolveKeySource() string {
	if opts.KeySource != "" {
		return opts.KeySource
	}
	if opts.EnvKey != "" {
		return KeySourceEnvKey
	}
	if opts.APIKey != "" {
		return KeySourceAPIKey
	}
	return KeySourceKeep
}

// KeySource is an instance-level Actions secret or variable that can hold a provider key.
type KeySource struct {
	Name string
	Kind string // KeySourceSecret or KeySourceVariable
}

// Kinds of key sources, used for display.
const (
	KeySourceSecret   = "secret"
	KeySourceVariable = "variable"
)

// KeySources lists the instance-level Actions secrets and variables an operator can
// point a provider at. Secrets are listed first: they are stored encrypted.
func KeySources(ctx context.Context) []KeySource {
	if !setting.Actions.Enabled {
		return nil
	}

	sources := make([]KeySource, 0)
	if secrets, err := db.Find[secret_model.Secret](ctx, secret_model.FindSecretsOptions{}); err == nil {
		for _, s := range secrets {
			sources = append(sources, KeySource{Name: s.Name, Kind: KeySourceSecret})
		}
	}
	if vars, err := actions_model.FindVariables(ctx, actions_model.FindVariablesOpts{}); err == nil {
		for _, v := range vars {
			sources = append(sources, KeySource{Name: v.Name, Kind: KeySourceVariable})
		}
	}

	slices.SortFunc(sources, func(a, b KeySource) int {
		if a.Kind != b.Kind {
			if a.Kind == KeySourceSecret {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return sources
}

// ModelFetchOptions describes an ad-hoc connection used to look up a provider's models.
type ModelFetchOptions struct {
	Provider string
	BaseURL  string
	APIKey   string // key typed in the form; takes precedence over EnvKey
	EnvKey   string // Actions secret/variable or environment variable holding the key
	Headers  map[string]string
}

// ResolveProviderAPIKey returns the key a stored provider would authenticate with.
// It is used when a form can not carry the secret, e.g. looking up models while editing.
func ResolveProviderAPIKey(ctx context.Context, p *agent_model.Provider) string {
	return resolveAPIKey(ctx, p)
}

// FetchModels lists the models a provider offers, without storing anything. The
// catalog is passed through as the provider reports it: picking from it is the
// operator's job.
func FetchModels(ctx context.Context, opts ModelFetchOptions) ([]agent_model.ProviderModel, error) {
	apiKey := opts.APIKey
	if apiKey == "" && opts.EnvKey != "" {
		apiKey = lookupVariable(ctx, opts.EnvKey)
	}
	if apiKey == "" {
		return nil, errors.New("no api key available")
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

// resolveAPIKey returns the API key to use, in order: the instance-level Actions
// variable named by EnvKey, the environment variable of the same name, then the
// stored encrypted key.
func resolveAPIKey(ctx context.Context, p *agent_model.Provider) string {
	if name := p.GetEnvKey(); name != "" {
		if v := lookupVariable(ctx, name); v != "" {
			return v
		}
	}
	if p.APIKeyEnc != "" {
		apiKey, err := secret_module.DecryptSecret(setting.SecretKey, p.APIKeyEnc)
		if err != nil {
			log.Warn("failed to decrypt provider key %d: %v", p.ID, err)
			return ""
		}
		return apiKey
	}
	return ""
}

// lookupVariable reads an instance-level Actions secret (encrypted) or variable
// (plain text) with the given name, falling back to the process environment
// (app.ini [env] is loaded into it at startup).
func lookupVariable(ctx context.Context, name string) string {
	if value, found := actionsValueAt(ctx, name); found {
		return value
	}
	return os.Getenv(name)
}

// actionsValueAt reads an instance-level (owner and repo both 0) Actions secret or
// variable. Secrets are preferred because variables are stored in plain text.
var actionsValueAt = func(ctx context.Context, name string) (string, bool) {
	if !setting.Actions.Enabled {
		return "", false
	}
	if secrets, err := db.Find[secret_model.Secret](ctx, secret_model.FindSecretsOptions{Name: name}); err == nil && len(secrets) > 0 {
		value, err := secret_module.DecryptSecret(setting.SecretKey, secrets[0].Data)
		if err != nil {
			log.Warn("failed to decrypt Actions secret %q: %v", name, err)
		} else {
			return value, true
		}
	}
	if vars, err := actions_model.FindVariables(ctx, actions_model.FindVariablesOpts{Name: name}); err == nil && len(vars) > 0 {
		return vars[0].Data, true
	}
	return "", false
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
	switch opts.resolveKeySource() {
	case KeySourceEnvKey:
		p.EnvKey = opts.EnvKey
		if p.EnvKey == "" {
			return nil, ErrNoKeySource{}
		}
	case KeySourceAPIKey:
		if opts.APIKey == "" {
			return nil, ErrNoKeySource{}
		}
		encrypted, err := secret_module.EncryptSecret(setting.SecretKey, opts.APIKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt api key: %w", err)
		}
		p.APIKeyEnc = encrypted
	default:
		return nil, ErrNoKeySource{}
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
		p.HasKey = p.APIKeyEnc != "" || p.GetEnvKey() != ""
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
		p.HasKey = p.APIKeyEnc != "" || p.GetEnvKey() != ""
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
	p.HasKey = p.APIKeyEnc != "" || p.GetEnvKey() != ""
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
	cols := []string{"name", "provider", "base_url", "model", "models_json", "updated_unix"}
	switch opts.resolveKeySource() {
	case KeySourceAPIKey:
		// the stored key becomes the source, so any configured variable is dropped
		p.EnvKey = ""
		cols = append(cols, "env_key")
		if opts.APIKey != "" {
			encrypted, err := secret_module.EncryptSecret(setting.SecretKey, opts.APIKey)
			if err != nil {
				return nil, fmt.Errorf("encrypt api key: %w", err)
			}
			p.APIKeyEnc = encrypted
			cols = append(cols, "api_key_enc")
		}
		if p.APIKeyEnc == "" {
			return nil, ErrNoKeySource{}
		}
	case KeySourceEnvKey:
		p.EnvKey = opts.EnvKey
		cols = append(cols, "env_key")
		if p.EnvKey == "" {
			return nil, ErrNoKeySource{}
		}
	case KeySourceKeep:
		// leave api_key_enc and env_key untouched
	default:
		return nil, ErrNoKeySource{}
	}
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

	apiKey := resolveAPIKey(ctx, p)
	if apiKey == "" {
		return nil, errors.New("no api key available")
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

// GetDecryptedAPIKey returns the decrypted API key for a provider.
func GetDecryptedAPIKey(ctx context.Context, id int64) (string, error) {
	p, err := GetProviderByID(ctx, id)
	if err != nil {
		return "", err
	}
	apiKey := resolveAPIKey(ctx, p)
	return apiKey, nil
}

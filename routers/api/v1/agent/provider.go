// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"net/http"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/modules/web"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/agent/llm"
	"gitea.dev/services/context"
)

// ProviderModelOption describes one selected model.
type ProviderModelOption struct {
	ID   string `json:"id" binding:"Required"`
	Name string `json:"name"`
	// Inputs are the input modalities the provider reports, read-only for clients.
	Inputs []string `json:"inputs,omitempty"`
}

// ProviderResponse is the public representation of a provider (API key masked).
type ProviderResponse struct {
	ID       int64                 `json:"id"`
	Name     string                `json:"name"`
	Provider string                `json:"provider"`
	BaseURL  string                `json:"base_url"`
	Model    string                `json:"model"` // first of Models, kept for compatibility
	Models   []ProviderModelOption `json:"models"`
	HasKey   bool                  `json:"has_key"`
	// HeaderNames lists configured custom request header names; values are never returned.
	HeaderNames []string `json:"header_names"`
	// EnvKey is the environment variable name used for the API key (if any).
	EnvKey  string `json:"env_key"`
	Created int64  `json:"created"`
}

// CreateProviderOption is the request body for creating a provider.
type CreateProviderOption struct {
	Name     string                `json:"name" binding:"Required"`
	Provider string                `json:"provider" binding:"Required"`
	BaseURL  string                `json:"base_url"`
	Model    string                `json:"model"` // single-model shorthand, superseded by Models
	Models   []ProviderModelOption `json:"models"`
	APIKey   string                `json:"api_key"`
	Headers  map[string]string     `json:"headers"`
	EnvKey   string                `json:"env_key"`
}

// UpdateProviderOption is the request body for updating a provider.
type UpdateProviderOption struct {
	Name     string                `json:"name" binding:"Required"`
	Provider string                `json:"provider" binding:"Required"`
	BaseURL  string                `json:"base_url"`
	Model    string                `json:"model"` // single-model shorthand, superseded by Models
	Models   []ProviderModelOption `json:"models"`
	APIKey   string                `json:"api_key"`
	Headers  map[string]string     `json:"headers"`
	EnvKey   string                `json:"env_key"`
}

// ListProviders returns all providers of this instance.
func ListProviders(ctx *context.APIContext) {
	providers, err := agent_service.GetProviders(ctx, "")
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	list := make([]*ProviderResponse, 0, len(providers))
	for _, p := range providers {
		list = append(list, toResponse(p))
	}
	ctx.JSON(http.StatusOK, struct {
		List  []*ProviderResponse `json:"list"`
		Total int64               `json:"total"`
	}{List: list, Total: int64(len(list))})
}

// GetProvider returns a single provider by ID.
func GetProvider(ctx *context.APIContext) {
	id := ctx.PathParamInt64("id")
	p, err := agent_service.GetProviderByID(ctx, id)
	if err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.APIError(http.StatusNotFound, "provider not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}
	ctx.JSON(http.StatusOK, toResponse(p))
}

// CreateProvider creates a new LLM provider.
func CreateProvider(ctx *context.APIContext) {
	form := web.GetForm[*CreateProviderOption](ctx)
	if ctx.Written() {
		return
	}

	if form.APIKey == "" && form.EnvKey == "" {
		ctx.APIError(http.StatusBadRequest, "either api_key or env_key must be provided")
		return
	}
	models := toModels(form.Models, form.Model)
	if len(models) == 0 {
		ctx.APIError(http.StatusBadRequest, "at least one model must be provided")
		return
	}
	if info, ok := llm.Provider(form.Provider); ok && info.RequiresBaseURL && form.BaseURL == "" {
		ctx.APIError(http.StatusBadRequest, "base_url is required for provider "+form.Provider)
		return
	}

	p, err := agent_service.CreateProvider(ctx, ctx.Doer.ID, agent_service.ProviderOptions{
		Name:     form.Name,
		Provider: form.Provider,
		BaseURL:  form.BaseURL,
		Models:   models,
		APIKey:   form.APIKey,
		Headers:  form.Headers,
		EnvKey:   form.EnvKey,
	})
	if err != nil {
		if errors.Is(err, agent_service.ErrUnsupportedProvider{}) {
			ctx.APIError(http.StatusBadRequest, err.Error())
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}
	ctx.JSON(http.StatusCreated, toResponse(p))
}

// UpdateProvider updates an existing LLM provider.
func UpdateProvider(ctx *context.APIContext) {
	id := ctx.PathParamInt64("id")
	form := web.GetForm[*UpdateProviderOption](ctx)
	if ctx.Written() {
		return
	}

	models := toModels(form.Models, form.Model)
	if len(models) == 0 {
		ctx.APIError(http.StatusBadRequest, "at least one model must be provided")
		return
	}

	p, err := agent_service.UpdateProvider(ctx, id, agent_service.ProviderOptions{
		Name:     form.Name,
		Provider: form.Provider,
		BaseURL:  form.BaseURL,
		Models:   models,
		APIKey:   form.APIKey,
		Headers:  form.Headers,
		EnvKey:   form.EnvKey,
	})
	if err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.APIError(http.StatusNotFound, "provider not found")
		} else if errors.Is(err, agent_service.ErrUnsupportedProvider{}) {
			ctx.APIError(http.StatusBadRequest, err.Error())
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}
	ctx.JSON(http.StatusOK, toResponse(p))
}

// DeleteProvider deletes an LLM provider.
func DeleteProvider(ctx *context.APIContext) {
	id := ctx.PathParamInt64("id")
	if err := agent_service.DeleteProvider(ctx, id); err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.APIError(http.StatusNotFound, "provider not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}
	ctx.JSON(http.StatusNoContent, nil)
}

// TestProvider tests the provider API key by sending a minimal chat request.
func TestProvider(ctx *context.APIContext) {
	id := ctx.PathParamInt64("id")
	res, err := agent_service.TestProvider(ctx, id, ctx.FormTrim("model"))
	if err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.APIError(http.StatusNotFound, "provider not found")
		} else {
			ctx.APIError(http.StatusBadRequest, "provider test failed: "+err.Error())
		}
		return
	}

	ctx.JSON(http.StatusOK, struct {
		OK    bool   `json:"ok"`
		Model string `json:"model"`
		Reply string `json:"reply"`
	}{OK: true, Model: res.Model, Reply: res.Reply})
}

// toModels prefers the models list and falls back to the single-model shorthand.
func toModels(models []ProviderModelOption, single string) []agent_model.ProviderModel {
	out := make([]agent_model.ProviderModel, 0, len(models)+1)
	for _, m := range models {
		out = append(out, agent_model.ProviderModel{ID: m.ID, Name: m.Name, Inputs: m.Inputs})
	}
	if len(out) == 0 && single != "" {
		out = append(out, agent_model.ProviderModel{ID: single})
	}
	return agent_model.NormalizeModels(out)
}

// toResponse converts a Provider model to a public response (secrets masked).
func toResponse(p *agent_model.Provider) *ProviderResponse {
	models := p.Models()
	modelOptions := make([]ProviderModelOption, 0, len(models))
	for _, m := range models {
		modelOptions = append(modelOptions, ProviderModelOption{ID: m.ID, Name: m.Name, Inputs: m.Inputs})
	}
	return &ProviderResponse{
		ID:          p.ID,
		Name:        p.Name,
		Provider:    p.Provider,
		BaseURL:     p.BaseURL,
		Model:       p.Model,
		Models:      modelOptions,
		HasKey:      p.APIKeyEnc != "" || p.EnvKey != "",
		HeaderNames: p.HeaderNames(),
		EnvKey:      p.EnvKey,
		Created:     int64(p.CreatedUnix),
	}
}

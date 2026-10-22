// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/agent/llm"
	"gitea.dev/services/context"
)

const (
	tplProviders    = "agent/providers"
	tplProviderForm = "agent/provider_form"

	// providersURL is the admin page path, relative to AppSubURL.
	providersURL = "/-/admin/agent/providers"
)

// providerForm holds the form values so a failed submit can be re-rendered.
// The API key itself is never echoed back into the page.
type providerForm struct {
	Name       string
	Provider   string
	BaseURL    string
	APIKeyID   int64
	ManualRaw  string
	HeadersRaw string
	Selected   []string
}

// HasSelected reports whether the given model id is already checked in the form.
func (f providerForm) HasSelected(id string) bool {
	return slices.Contains(f.Selected, id)
}

func redirectToProviders(ctx *context.Context) {
	ctx.Redirect(setting.AppSubURL + providersURL)
}

// ProviderNewModelsRedirect sends a refreshed result page back to the create form.
func ProviderNewModelsRedirect(ctx *context.Context) {
	ctx.Redirect(setting.AppSubURL + providersURL + "/new")
}

func editProvidersRedirect(ctx *context.Context) {
	ctx.Redirect(fmt.Sprintf("%s%s/%d/edit", setting.AppSubURL, providersURL, ctx.PathParamInt64("id")))
}

// Providers renders the BYOK provider management page.
func Providers(ctx *context.Context) {
	names, err := agent_service.APIKeyNames(ctx)
	if err != nil {
		ctx.ServerError("APIKeyNames", err)
		return
	}
	ctx.Data["Title"] = ctx.Tr("agent.providers_title")
	ctx.Data["PageIsAgentProviders"] = true
	ctx.Data["APIKeyNames"] = names

	page := max(ctx.FormInt("page"), 1)
	keyword := ctx.FormTrim("q")
	ctx.Data["Keyword"] = keyword

	pageSize := setting.UI.Admin.OrgPagingNum
	providers, total, err := agent_service.FindProviders(ctx, agent_model.FindProvidersOptions{
		ListOptions: db.ListOptions{Page: page, PageSize: pageSize},
		Keyword:     keyword,
	})
	if err != nil {
		ctx.ServerError("FindProviders", err)
		return
	}

	ctx.Data["Providers"] = providers
	ctx.Data["Total"] = total
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(pageSize).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplProviders)
}

// ProviderNew renders the form for adding a provider.
func ProviderNew(ctx *context.Context) {
	renderProviderForm(ctx, nil, providerForm{}, nil, "")
}

// ProviderNewPost creates a provider from the form.
func ProviderNewPost(ctx *context.Context) {
	form := providerFormFrom(ctx)
	headers, err := agent_service.ParseHeaderLines(form.HeadersRaw)
	if err != nil {
		renderProviderForm(ctx, nil, form, catalogFrom(ctx), err.Error())
		return
	}

	models := modelsFrom(ctx, form)
	if err := validateProviderForm(ctx, form, models); err != nil {
		renderProviderForm(ctx, nil, form, catalogFrom(ctx), err.Error())
		return
	}

	if _, err := agent_service.CreateProvider(ctx, ctx.Doer.ID, agent_service.ProviderOptions{
		Name:     form.Name,
		Provider: form.Provider,
		BaseURL:  form.BaseURL,
		Models:   models,
		APIKeyID: form.APIKeyID,
		Headers:  headers,
	}); err != nil {
		renderProviderForm(ctx, nil, form, catalogFrom(ctx), err.Error())
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.added_success"))
	redirectToProviders(ctx)
}

// ProviderNewFetchModels queries the provider for its models and re-renders the form.
func ProviderNewFetchModels(ctx *context.Context) {
	fetchModels(ctx, nil)
}

// ProviderEdit renders the form for an existing provider.
func ProviderEdit(ctx *context.Context) {
	p, err := agent_service.GetProviderByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.ServerError("GetProviderByID", err)
		return
	}

	renderProviderForm(ctx, p, providerForm{
		Name:       p.Name,
		Provider:   p.Provider,
		BaseURL:    p.BaseURL,
		APIKeyID:   p.APIKeyID,
		HeadersRaw: agent_service.FormatHeaderLines(p.CustomHeaders()),
		Selected:   p.ModelIDs(),
	}, p.Models(), "")
}

// ProviderEditPost updates an existing provider.
func ProviderEditPost(ctx *context.Context) {
	p, err := agent_service.GetProviderByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.ServerError("GetProviderByID", err)
		return
	}

	form := providerFormFrom(ctx)
	headers, err := agent_service.ParseHeaderLines(form.HeadersRaw)
	if err != nil {
		renderProviderForm(ctx, p, form, catalogFrom(ctx), err.Error())
		return
	}

	models := modelsFrom(ctx, form)
	if err := validateProviderForm(ctx, form, models); err != nil {
		renderProviderForm(ctx, p, form, catalogFrom(ctx), err.Error())
		return
	}

	if _, err := agent_service.UpdateProvider(ctx, p.ID, agent_service.ProviderOptions{
		Name:     form.Name,
		Provider: form.Provider,
		BaseURL:  form.BaseURL,
		Models:   models,
		APIKeyID: form.APIKeyID,
		Headers:  headers,
	}); err != nil {
		renderProviderForm(ctx, p, form, catalogFrom(ctx), err.Error())
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.updated_success"))
	redirectToProviders(ctx)
}

// ProviderEditModelsRedirect sends a refreshed result page back to the edit form.
func ProviderEditModelsRedirect(ctx *context.Context) {
	editProvidersRedirect(ctx)
}

// ProviderEditFetchModels queries the provider for its models and re-renders the edit form.
func ProviderEditFetchModels(ctx *context.Context) {
	p, err := agent_service.GetProviderByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.ServerError("GetProviderByID", err)
		return
	}
	fetchModels(ctx, p)
}

func fetchModels(ctx *context.Context, p *agent_model.Provider) {
	form := providerFormFrom(ctx)
	headers, err := agent_service.ParseHeaderLines(form.HeadersRaw)
	if err != nil {
		renderProviderForm(ctx, p, form, nil, err.Error())
		return
	}

	// an edit form without a fresh pick keeps the key already bound to the provider
	apiKeyID := form.APIKeyID
	if apiKeyID == 0 && p != nil {
		apiKeyID = p.APIKeyID
	}

	models, err := agent_service.FetchModels(ctx, agent_service.ModelFetchOptions{
		Provider: form.Provider,
		BaseURL:  form.BaseURL,
		APIKeyID: apiKeyID,
		Headers:  headers,
	})
	if err != nil {
		renderProviderForm(ctx, p, form, nil, ctx.Locale.TrString("agent.model_fetch_failed", err.Error()))
		return
	}
	if len(models) == 0 {
		renderProviderForm(ctx, p, form, nil, ctx.Locale.TrString("agent.model_fetch_empty"))
		return
	}

	ctx.Data["Notice"] = ctx.Tr("agent.model_fetch_success", len(models))
	renderProviderForm(ctx, p, form, models, "")
}

// ProviderDelete removes a provider owned by the current user.
func ProviderDelete(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	if err := agent_service.DeleteProvider(ctx, id); err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("agent.deleted_success"))
	}
	redirectToProviders(ctx)
}

// ProviderTest sends a minimal chat request with the stored API key.
func ProviderTest(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	res, err := agent_service.TestProvider(ctx, id, "")
	if err != nil {
		if errors.Is(err, agent_model.ErrProviderNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.Flash.Error(ctx.Tr("agent.test_failed", err.Error()))
	} else {
		log.Trace("agent provider %d test ok (model %s): %s", id, res.Model, res.Reply)
		ctx.Flash.Success(ctx.Tr("agent.test_success"))
	}
	redirectToProviders(ctx)
}

func providerFormFrom(ctx *context.Context) providerForm {
	return providerForm{
		Name:       ctx.FormTrim("name"),
		Provider:   ctx.FormTrim("provider"),
		BaseURL:    ctx.FormTrim("base_url"),
		APIKeyID:   ctx.FormInt64("api_key_id"),
		ManualRaw:  ctx.FormString("manual_models"),
		HeadersRaw: ctx.FormString("headers"),
		Selected:   ctx.FormStrings("models"),
	}
}

// modelsFrom combines the checked models (named and described via the catalog) with
// the manual entries.
func modelsFrom(ctx *context.Context, form providerForm) []agent_model.ProviderModel {
	known := map[string]agent_model.ProviderModel{}
	for _, m := range catalogFrom(ctx) {
		known[m.ID] = m
	}

	models := make([]agent_model.ProviderModel, 0, len(form.Selected)+1)
	for _, id := range form.Selected {
		models = append(models, agent_model.ProviderModel{ID: id, Name: known[id].Name, Inputs: known[id].Inputs})
	}
	models = append(models, agent_service.ParseModelLines(form.ManualRaw)...)
	return agent_model.NormalizeModels(models)
}

// validateProviderForm checks the form; on edit p carries the provider being changed.
func validateProviderForm(ctx *context.Context, form providerForm, models []agent_model.ProviderModel) error {
	if form.Name == "" || form.Provider == "" {
		return errors.New(ctx.Locale.TrString("agent.required_fields"))
	}
	info, ok := llm.Provider(form.Provider)
	if !ok {
		return agent_service.ErrUnsupportedProvider{Type: form.Provider}
	}
	if info.RequiresBaseURL && form.BaseURL == "" {
		return errors.New(ctx.Locale.TrString("agent.base_url_required"))
	}
	if len(models) == 0 {
		return errors.New(ctx.Locale.TrString("agent.models_required"))
	}

	if form.APIKeyID != 0 {
		if _, err := agent_service.GetAPIKey(ctx, form.APIKeyID); err != nil {
			return errors.New(ctx.Locale.TrString("agent.api_key_invalid"))
		}
	}
	return nil
}

func catalogFrom(ctx *context.Context) []agent_model.ProviderModel {
	raw := ctx.FormString("model_catalog")
	if raw == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	models := []agent_model.ProviderModel{}
	if err := json.Unmarshal(data, &models); err != nil {
		return nil
	}
	return models
}

func encodeCatalog(models []agent_model.ProviderModel) string {
	if len(models) == 0 {
		return ""
	}
	data, err := json.Marshal(models)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

// renderProviderForm renders the create form when p is nil, the edit form otherwise.
func renderProviderForm(ctx *context.Context, p *agent_model.Provider, form providerForm, catalog []agent_model.ProviderModel, errMsg string) {
	providerTypes := llm.Providers()
	if form.Provider == "" && len(providerTypes) > 0 {
		form.Provider = providerTypes[0].Type
	}
	isEdit := p != nil
	formAction := providersURL + "/new"
	fetchAction := formAction + "/models"
	if isEdit {
		formAction = fmt.Sprintf("%s/%d/edit", providersURL, p.ID)
		fetchAction = formAction + "/models"
	}

	ctx.Data["Title"] = ctx.Tr("agent.add_provider")
	ctx.Data["PageIsAgentProviders"] = true
	ctx.Data["ProviderTypes"] = providerTypes
	keys, err := agent_service.ListAPIKeys(ctx)
	if err != nil {
		ctx.ServerError("ListAPIKeys", err)
		return
	}
	ctx.Data["APIKeys"] = keys
	ctx.Data["Form"] = form
	ctx.Data["Catalog"] = catalog
	ctx.Data["CatalogEncoded"] = encodeCatalog(catalog)
	ctx.Data["ErrorMsg"] = errMsg
	ctx.Data["IsEdit"] = isEdit
	ctx.Data["FormAction"] = setting.AppSubURL + formAction
	ctx.Data["FetchAction"] = setting.AppSubURL + fetchAction
	ctx.Data["CancelLink"] = setting.AppSubURL + providersURL
	if isEdit {
		ctx.Data["Title"] = ctx.Tr("agent.edit_provider")
		ctx.Data["Provider"] = p
	}
	ctx.HTML(http.StatusOK, tplProviderForm)
}

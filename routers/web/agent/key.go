// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"net/http"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

const (
	tplAPIKeys = "agent/keys"
	tplAPIKey  = "agent/key_form"

	apiKeysURL = "/-/admin/agent/keys"
)

func redirectToAPIKeys(ctx *context.Context) {
	ctx.Redirect(setting.AppSubURL + apiKeysURL)
}

// APIKeys renders the credential store. Stored values are never rendered, only names.
func APIKeys(ctx *context.Context) {
	page := max(ctx.FormInt("page"), 1)
	keyword := ctx.FormTrim("q")

	pageSize := setting.UI.Admin.OrgPagingNum
	keys, total, err := agent_service.FindAPIKeys(ctx, agent_model.FindAPIKeysOptions{
		ListOptions: db.ListOptions{Page: page, PageSize: pageSize},
		Keyword:     keyword,
	})
	if err != nil {
		ctx.ServerError("FindAPIKeys", err)
		return
	}
	usage, err := agent_service.APIKeyUsage(ctx)
	if err != nil {
		ctx.ServerError("APIKeyUsage", err)
		return
	}

	ctx.Data["Title"] = ctx.Tr("agent.api_keys_title")
	ctx.Data["PageIsAgentAPIKeys"] = true
	ctx.Data["Keyword"] = keyword
	ctx.Data["APIKeys"] = keys
	ctx.Data["Usage"] = usage
	ctx.Data["Total"] = total
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(pageSize).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplAPIKeys)
}

// APIKeyNew renders the form that stores a new key.
func APIKeyNew(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agent.api_key_add")
	ctx.Data["PageIsAgentAPIKeys"] = true
	ctx.Data["IsEdit"] = false
	ctx.Data["FormAction"] = setting.AppSubURL + apiKeysURL + "/new"
	ctx.Data["CancelLink"] = setting.AppSubURL + apiKeysURL
	ctx.HTML(http.StatusOK, tplAPIKey)
}

// APIKeyNewPost stores a new credential; its value is only ever accepted, never returned.
func APIKeyNewPost(ctx *context.Context) {
	_, err := agent_service.CreateAPIKey(ctx, ctx.Doer.ID, ctx.FormTrim("name"), ctx.FormString("value"))
	if err != nil {
		var invalid agent_service.ErrAPIKeyInvalid
		if errors.As(err, &invalid) {
			ctx.Flash.Error(err.Error())
		} else {
			ctx.Flash.Error(ctx.Tr("agent.api_key_save_failed", err.Error()))
		}
		ctx.Redirect(setting.AppSubURL + apiKeysURL + "/new")
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.api_key_added"))
	redirectToAPIKeys(ctx)
}

// APIKey renders the form that replaces the value of an existing key.
func APIKey(ctx *context.Context) {
	key, err := agent_service.GetAPIKey(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		apiKeyError(ctx, err)
		return
	}
	ctx.Data["Title"] = ctx.Tr("agent.api_key_edit")
	ctx.Data["PageIsAgentAPIKeys"] = true
	ctx.Data["IsEdit"] = true
	ctx.Data["APIKey"] = key
	ctx.Data["FormAction"] = setting.AppSubURL + apiKeysURL + "/" + ctx.PathParam("id")
	ctx.Data["CancelLink"] = setting.AppSubURL + apiKeysURL
	ctx.HTML(http.StatusOK, tplAPIKey)
}

// APIKeyPost replaces the value of an existing key, keeping every binding to it.
func APIKeyPost(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	if err := agent_service.ReplaceAPIKeyValue(ctx, id, ctx.FormString("value")); err != nil {
		var invalid agent_service.ErrAPIKeyInvalid
		if errors.As(err, &invalid) {
			ctx.Flash.Error(err.Error())
		} else if errors.Is(err, agent_service.ErrAPIKeyNotFound{}) {
			ctx.NotFound(nil)
			return
		} else {
			ctx.Flash.Error(ctx.Tr("agent.api_key_save_failed", err.Error()))
		}
		ctx.Redirect(setting.AppSubURL + apiKeysURL + "/" + ctx.PathParam("id"))
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.api_key_updated"))
	redirectToAPIKeys(ctx)
}

// APIKeyDelete removes a credential that no provider uses any more.
func APIKeyDelete(ctx *context.Context) {
	if err := agent_service.DeleteAPIKey(ctx, ctx.PathParamInt64("id")); err != nil {
		switch {
		case errors.Is(err, agent_service.ErrAPIKeyNotFound{}):
			ctx.NotFound(nil)
		case errors.Is(err, agent_service.ErrAPIKeyInUse{}):
			ctx.Flash.Error(ctx.Tr("agent.api_key_in_use"))
			redirectToAPIKeys(ctx)
		default:
			ctx.ServerError("DeleteAPIKey", err)
		}
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.api_key_deleted"))
	redirectToAPIKeys(ctx)
}

func apiKeyError(ctx *context.Context, err error) {
	if errors.Is(err, agent_service.ErrAPIKeyNotFound{}) {
		ctx.NotFound(nil)
		return
	}
	ctx.ServerError("agent api key", err)
}

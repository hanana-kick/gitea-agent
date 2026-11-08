// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"net/http"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

const (
	tplModelRoles = "agent/roles"

	// modelRolesURL is the admin page path, relative to AppSubURL.
	modelRolesURL = "/-/admin/agent/roles"
)

// ModelRoleRow is one role with the model assigned to it.
type ModelRoleRow struct {
	Role string
	Ref  string
}

// ModelRoles shows which model each role of the agent answers with.
func ModelRoles(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agent.roles_title")
	ctx.Data["PageIsAgentModelRoles"] = true

	refs, err := agent_service.GetModelRoleRefs(ctx)
	if err != nil {
		ctx.ServerError("GetModelRoleRefs", err)
		return
	}
	rows := make([]ModelRoleRow, 0, len(agent_model.ModelRoleOptions))
	for _, role := range agent_model.ModelRoleOptions {
		rows = append(rows, ModelRoleRow{Role: role, Ref: refs[role]})
	}
	options, err := agent_service.ModelOptions(ctx, 200)
	if err != nil {
		ctx.ServerError("ModelOptions", err)
		return
	}
	ctx.Data["Rows"] = rows
	ctx.Data["ModelOptions"] = options
	ctx.HTML(http.StatusOK, tplModelRoles)
}

// ModelRolesPost stores the model of every role.
func ModelRolesPost(ctx *context.Context) {
	for _, role := range agent_model.ModelRoleOptions {
		ref := ctx.FormTrim("role_" + role)
		if ref != "" {
			allowed, err := agent_service.CanUseModelRef(ctx, ref)
			if err != nil {
				ctx.ServerError("CanUseModelRef", err)
				return
			}
			if !allowed {
				ctx.Flash.Error(ctx.Tr("agent.roles_invalid", ctx.Tr("agent.role_"+role)))
				ctx.Redirect(setting.AppSubURL + modelRolesURL)
				return
			}
		}
		if err := agent_service.SetModelRoleRef(ctx, role, ref); err != nil {
			ctx.ServerError("SetModelRoleRef", err)
			return
		}
	}
	ctx.Flash.Success(ctx.Tr("agent.roles_saved"))
	ctx.Redirect(setting.AppSubURL + modelRolesURL)
}

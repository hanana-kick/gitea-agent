// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"net/http"
	"slices"

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
	// Light marks the roles that carry the small jobs, which are the ones a cheap model answers.
	Light bool
	// Class is what the role needs of its model: a decision, or the writing itself.
	Class string
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
		rows = append(rows, ModelRoleRow{
			Role: role, Ref: refs[role], Light: agent_model.IsLightModelRole(role),
			Class: agent_model.ModelRoleClassOf(role),
		})
	}
	options, err := agent_service.ModelOptions(ctx, 200)
	if err != nil {
		ctx.ServerError("ModelOptions", err)
		return
	}
	// each class of role is offered the models that serve it, and an instance with no decision
	// model is offered every model for that role
	optionsByClass, err := agent_service.ModelOptionsByClass(ctx, 200)
	if err != nil {
		ctx.ServerError("ModelOptionsByClass", err)
		return
	}
	ctx.Data["NoDecisionModels"] = !slices.ContainsFunc(options, func(o agent_service.ModelOption) bool { return o.Decisions })
	ctx.Data["OptionsByClass"] = optionsByClass
	ctx.Data["Presets"] = agent_service.RolePresets
	ctx.Data["RoleClasses"] = agent_model.ModelRoleClasses
	byClass := make(map[string][]ModelRoleRow, len(agent_model.ModelRoleClasses))
	for _, class := range agent_model.ModelRoleClasses {
		byClass[class] = make([]ModelRoleRow, 0, len(rows))
	}
	for _, row := range rows {
		byClass[row.Class] = append(byClass[row.Class], row)
	}
	ctx.Data["RowsByClass"] = byClass
	ctx.HTML(http.StatusOK, tplModelRoles)
}

// ModelRolesPreset assigns every role at once: a cheap model for the small jobs and a strong one
// for the work, split the way the preset says.
func ModelRolesPreset(ctx *context.Context) {
	preset := ctx.FormTrim("preset")
	models := agent_service.RoleModels{
		Cheap:    ctx.FormTrim("cheap"),
		Strong:   ctx.FormTrim("strong"),
		Decision: ctx.FormTrim("decision"),
	}
	for _, ref := range []string{models.Cheap, models.Strong, models.Decision} {
		if ref == "" {
			continue
		}
		allowed, err := agent_service.CanUseModelRef(ctx, ref)
		if err != nil {
			ctx.ServerError("CanUseModelRef", err)
			return
		}
		if !allowed {
			ctx.Flash.Error(ctx.Tr("agent.roles_preset_unknown_model"))
			ctx.Redirect(modelRolesURL)
			return
		}
	}
	if err := agent_service.AssignRoles(ctx, preset, models); err != nil {
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("agent.roles_preset_applied", ctx.Tr("agent.roles_preset_"+preset)))
	}
	ctx.Redirect(modelRolesURL)
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

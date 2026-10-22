// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"net/http"

	"gitea.dev/modules/setting"
	agent_skills "gitea.dev/services/agent/skills"
	"gitea.dev/services/context"
)

const (
	tplAgentSkills = "agent/skills"

	// agentSkillsURL is the admin page path, relative to AppSubURL.
	agentSkillsURL = "/-/admin/agent/skills"
)

// SkillRow is one skill as the operator page shows it.
type SkillRow struct {
	Name        string
	Description string
	Body        string
	Scope       string
	Enabled     bool
}

// Skills lists the skills the agent may open, with the scope each one reaches.
func Skills(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agent.skills_title")
	ctx.Data["PageIsAgentSkills"] = true

	list, err := agent_skills.All(ctx)
	if err != nil {
		ctx.ServerError("All", err)
		return
	}
	rows := make([]SkillRow, 0, len(list))
	for _, skill := range list {
		rows = append(rows, SkillRow{
			Name:        skill.Name,
			Description: skill.Description,
			Body:        skill.Body,
			Scope:       skill.Scope,
			Enabled:     skill.Enabled,
		})
	}
	ctx.Data["Rows"] = rows
	ctx.HTML(http.StatusOK, tplAgentSkills)
}

// SkillsPost stores one skill: the name identifies it, so saving an existing name edits it.
func SkillsPost(ctx *context.Context) {
	err := agent_skills.Save(ctx,
		ctx.FormString("name"),
		ctx.FormString("description"),
		ctx.FormString("body"),
		ctx.FormString("scope"),
		ctx.FormString("enabled") == "true",
	)
	if err != nil {
		if skillsErrorText, ok := err.(agent_skills.ErrSkillInvalid); ok {
			ctx.Flash.Error(ctx.Tr("agent.skills_invalid", skillsErrorText.Reason))
		} else {
			ctx.Flash.Error(err.Error())
		}
	} else {
		ctx.Flash.Success(ctx.Tr("agent.skills_saved"))
	}
	ctx.Redirect(setting.AppSubURL + agentSkillsURL)
}

// SkillsDelete removes a skill the agent may no longer open.
func SkillsDelete(ctx *context.Context) {
	name := ctx.FormString("name")
	if name == "" {
		ctx.Flash.Error(ctx.Tr("agent.skills_invalid", "the skill needs a name"))
	} else if err := agent_skills.Delete(ctx, name); err != nil {
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("agent.skills_deleted", name))
	}
	ctx.Redirect(setting.AppSubURL + agentSkillsURL)
}

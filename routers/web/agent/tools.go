// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"net/http"

	"gitea.dev/modules/setting"
	agent_tools "gitea.dev/services/agent/tools"
	"gitea.dev/services/context"
)

const (
	tplAgentTools = "agent/tools"

	// agentToolsURL is the admin page path, relative to AppSubURL.
	agentToolsURL = "/-/admin/agent/tools"
)

// ToolRow is one tool of the registry with the permission the admin gave it.
type ToolRow struct {
	Name        string
	Description string
	Scope       string
	Mode        string
	Enabled     bool
}

// Tools lists every tool the agent may be given, with its permission.
func Tools(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agent.tools_title")
	ctx.Data["PageIsAgentTools"] = true

	list := agent_tools.Registry()
	rows := make([]ToolRow, 0, len(list))
	for _, tool := range list {
		mode, enabled, err := agent_tools.ModeOf(ctx, tool.Name())
		if err != nil {
			ctx.ServerError("ModeOf", err)
			return
		}
		rows = append(rows, ToolRow{
			Name:        tool.Name(),
			Description: tool.Description(),
			Scope:       string(tool.Scope()),
			Mode:        mode,
			Enabled:     enabled,
		})
	}
	ctx.Data["Rows"] = rows
	ctx.HTML(http.StatusOK, tplAgentTools)
}

// ToolsPost stores the permission of every tool, so a tool taken out of the registry
// since the page was rendered keeps no stale row.
func ToolsPost(ctx *context.Context) {
	for _, tool := range agent_tools.Registry() {
		name := tool.Name()
		mode := ctx.FormTrim("mode_" + name)
		switch mode {
		case agent_tools.PermissionManual, agent_tools.PermissionAllowAll:
		default:
			mode = agent_tools.PermissionLLM
		}
		// an unchecked box submits nothing, so a missing value means disabled
		enabled := ctx.FormString("enabled_"+name) == "true"
		if err := agent_tools.SetMode(ctx, name, mode, enabled); err != nil {
			ctx.ServerError("SetMode", err)
			return
		}
	}
	ctx.Flash.Success(ctx.Tr("agent.tools_saved"))
	ctx.Redirect(setting.AppSubURL + agentToolsURL)
}

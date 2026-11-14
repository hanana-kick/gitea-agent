// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/modules/setting"
	agent_mcp "gitea.dev/services/agent/mcp"
	"gitea.dev/services/context"
)

const (
	tplMCPServers = "agent/mcp"
	tplMCPForm    = "agent/mcp_form"
	mcpURL        = "/-/admin/agent/mcp"
)

// mcpServerRow is one server as the page reads it, with what its tools are called.
type mcpServerRow struct {
	ID          int64
	Name        string
	Slug        string
	URL         string
	Description string
	HasToken    bool
	ReadOnly    bool
	Enabled     bool
	ToolPrefix  string
	Created     string
}

// MCPServers renders the servers an instance's agent may call.
func MCPServers(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agent.mcp_title")
	ctx.Data["PageIsAgentMCP"] = true

	page := max(ctx.FormInt("page"), 1)
	keyword := ctx.FormTrim("q")
	ctx.Data["Keyword"] = keyword
	pageSize := setting.UI.Admin.OrgPagingNum

	servers, total, err := agent_mcp.ListServers(ctx, keyword, page, pageSize)
	if err != nil {
		ctx.ServerError("ListServers", err)
		return
	}
	rows := make([]*mcpServerRow, 0, len(servers))
	for _, server := range servers {
		rows = append(rows, &mcpServerRow{
			ID: server.ID, Name: server.Name, Slug: server.Slug, URL: server.URL,
			Description: server.Description, HasToken: server.TokenEnc != "",
			ReadOnly: server.ReadOnly, Enabled: server.Enabled,
			ToolPrefix: agent_mcp.ToolName(server.Slug, ""),
			Created:    server.CreatedUnix.AsTime().Format("2006-01-02"),
		})
	}
	ctx.Data["Servers"] = rows
	ctx.Data["Total"] = total
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(pageSize).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplMCPServers)
}

// MCPServerNew renders the form for adding a server.
func MCPServerNew(ctx *context.Context) {
	renderMCPForm(ctx, nil, mcpForm{Enabled: true}, "")
}

// MCPServerNewPost stores a server.
func MCPServerNewPost(ctx *context.Context) {
	form := mcpFormFrom(ctx)
	server, err := agent_mcp.CreateServer(ctx, form.options())
	if err != nil {
		renderMCPForm(ctx, nil, form, err.Error())
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.mcp_added", server.Name))
	ctx.Redirect(setting.AppSubURL + mcpURL)
}

// MCPServerEdit renders the form for a server that is already there.
func MCPServerEdit(ctx *context.Context) {
	server, err := agent_mcp.GetServer(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		mcpError(ctx, err)
		return
	}
	renderMCPForm(ctx, server, mcpForm{
		Name: server.Name, URL: server.URL, Description: server.Description,
		ReadOnly: server.ReadOnly, Enabled: server.Enabled, HasToken: server.TokenEnc != "",
	}, "")
}

// MCPServerEditPost stores what was edited.
func MCPServerEditPost(ctx *context.Context) {
	server, err := agent_mcp.GetServer(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		mcpError(ctx, err)
		return
	}
	form := mcpFormFrom(ctx)
	updated, err := agent_mcp.UpdateServer(ctx, server.ID, form.options())
	if err != nil {
		renderMCPForm(ctx, server, form, err.Error())
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.mcp_updated", updated.Name))
	ctx.Redirect(setting.AppSubURL + mcpURL)
}

// MCPServerDelete takes a server away.
func MCPServerDelete(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	if err := agent_mcp.DeleteServer(ctx, id); err != nil {
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("agent.mcp_deleted"))
	}
	ctx.Redirect(setting.AppSubURL + mcpURL)
}

// MCPServerTest reads what a server offers, so an administrator sees it before switching a tool on.
func MCPServerTest(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	tools, err := agent_mcp.ServerTools(ctx, id)
	if err != nil {
		ctx.Flash.Error(ctx.Tr("agent.mcp_test_failed", err.Error()))
		ctx.Redirect(setting.AppSubURL + mcpURL)
		return
	}
	if len(tools) == 0 {
		ctx.Flash.Warning(ctx.Tr("agent.mcp_test_empty"))
		ctx.Redirect(setting.AppSubURL + mcpURL)
		return
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	ctx.Flash.Success(ctx.Tr("agent.mcp_test_success", len(tools), strings.Join(names, ", ")))
	ctx.Redirect(setting.AppSubURL + mcpURL)
}

// mcpForm holds the submitted values, so a failed submit can be rendered again.
type mcpForm struct {
	Name        string
	URL         string
	Description string
	ReadOnly    bool
	Enabled     bool
	ClearToken  bool
	HasToken    bool
	token       string
}

func (f mcpForm) options() agent_mcp.ServerOptions {
	return agent_mcp.ServerFromOptions(f.Name, f.URL, f.Description, f.token, f.ClearToken, f.ReadOnly, f.Enabled)
}

func mcpFormFrom(ctx *context.Context) mcpForm {
	return mcpForm{
		Name:        ctx.FormTrim("name"),
		URL:         ctx.FormTrim("url"),
		Description: ctx.FormTrim("description"),
		ReadOnly:    ctx.FormBool("read_only"),
		Enabled:     ctx.FormBool("enabled"),
		ClearToken:  ctx.FormBool("clear_token"),
		token:       ctx.FormTrim("token"),
	}
}

// renderMCPForm renders the create form when server is nil, the edit form otherwise.
func renderMCPForm(ctx *context.Context, server *agent_mcp.Server, form mcpForm, errMsg string) {
	isEdit := server != nil
	ctx.Data["Title"] = ctx.Tr("agent.mcp_new")
	ctx.Data["PageIsAgentMCP"] = true
	if isEdit {
		ctx.Data["Title"] = ctx.Tr("agent.mcp_edit")
	}
	ctx.Data["Server"] = server
	ctx.Data["IsEdit"] = isEdit
	ctx.Data["Form"] = form
	ctx.Data["ErrorMsg"] = errMsg
	ctx.Data["ToolPrefix"] = ""
	if isEdit {
		ctx.Data["ToolPrefix"] = agent_mcp.ToolName(server.Slug, "")
	}
	ctx.Data["FormAction"] = setting.AppSubURL + mcpURL + "/new"
	if isEdit {
		ctx.Data["FormAction"] = setting.AppSubURL + mcpURL + "/" + strconv.FormatInt(server.ID, 10) + "/edit"
	}
	ctx.Data["CancelLink"] = setting.AppSubURL + mcpURL
	ctx.HTML(http.StatusOK, tplMCPForm)
}

// mcpError answers a request about a server that is not there.
func mcpError(ctx *context.Context, err error) {
	if errors.Is(err, agent_model.ErrMCPServerNotFound{}) {
		ctx.NotFound(err)
		return
	}
	ctx.ServerError("MCP server", err)
}

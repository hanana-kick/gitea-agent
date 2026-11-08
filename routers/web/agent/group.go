// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

const (
	tplGroups    = "agent/groups"
	tplGroupForm = "agent/group_form"

	// groupsURL is the admin page path, relative to AppSubURL.
	groupsURL = "/-/admin/agent/groups"
)

func redirectToGroups(ctx *context.Context) {
	ctx.Redirect(setting.AppSubURL + groupsURL)
}

// ModelGroups renders the model group management page.
func ModelGroups(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agent.groups_title")
	ctx.Data["PageIsAgentModelGroups"] = true

	page := max(ctx.FormInt("page"), 1)
	keyword := ctx.FormTrim("q")
	ctx.Data["Keyword"] = keyword

	pageSize := setting.UI.Admin.OrgPagingNum
	roles, total, err := agent_service.FindModelGroups(ctx, agent_model.FindModelGroupsOptions{
		ListOptions: db.ListOptions{Page: page, PageSize: pageSize},
		Keyword:     keyword,
	})
	if err != nil {
		ctx.ServerError("FindModelGroups", err)
		return
	}

	ctx.Data["Roles"] = roles
	ctx.Data["Total"] = total
	ctx.Data["MemberCounts"] = agent_service.GroupMemberCounts(ctx)
	ctx.Data["Page"] = context.NewPagerBuilder(ctx).TotalCount(total).PerPageLimit(pageSize).CurPage(page).Build()
	ctx.HTML(http.StatusOK, tplGroups)
}

// ModelGroupNew renders the form for adding a group.
func ModelGroupNew(ctx *context.Context) {
	renderGroupForm(ctx, nil, groupForm{}, "")
}

// ModelGroupNewPost creates a group.
func ModelGroupNewPost(ctx *context.Context) {
	form := groupFormFrom(ctx)
	members, err := form.memberTargets()
	if err != nil {
		renderGroupForm(ctx, nil, form, err.Error())
		return
	}
	role, err := agent_service.CreateModelGroup(ctx, agent_service.ModelGroupOptions{
		Name:        form.Name,
		Description: form.Description,
		Mode:        form.Mode,
		Members:     members,
	})
	if err != nil {
		renderGroupForm(ctx, nil, form, err.Error())
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.group_added", role.Name))
	redirectToGroups(ctx)
}

// ModelGroupEdit renders the form for an existing role.
func ModelGroupEdit(ctx *context.Context) {
	role, err := agent_service.GetModelGroupByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		if errors.Is(err, agent_model.ErrModelGroupNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.ServerError("GetModelGroupByID", err)
		return
	}
	members, err := agent_service.GetGroupMembers(ctx, role.ID)
	if err != nil {
		ctx.ServerError("GetGroupMembers", err)
		return
	}
	renderGroupForm(ctx, role, groupForm{
		Name:        role.Name,
		Description: role.Description,
		Mode:        role.ModeOrDefault(),
		Members:     groupMemberValues(members),
	}, "")
}

// ModelGroupEditPost updates a group.
func ModelGroupEditPost(ctx *context.Context) {
	role, err := agent_service.GetModelGroupByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		if errors.Is(err, agent_model.ErrModelGroupNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.ServerError("GetModelGroupByID", err)
		return
	}

	form := groupFormFrom(ctx)
	members, err := form.memberTargets()
	if err != nil {
		renderGroupForm(ctx, role, form, err.Error())
		return
	}
	updated, err := agent_service.UpdateModelGroup(ctx, role.ID, agent_service.ModelGroupOptions{
		Name:        form.Name,
		Description: form.Description,
		Mode:        form.Mode,
		Members:     members,
	})
	if err != nil {
		renderGroupForm(ctx, role, form, err.Error())
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.group_updated", updated.Name))
	redirectToGroups(ctx)
}

// ModelGroupDelete removes a group.
func ModelGroupDelete(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	if err := agent_service.DeleteModelGroup(ctx, id); err != nil {
		if errors.Is(err, agent_model.ErrModelGroupNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("agent.group_deleted"))
	}
	redirectToGroups(ctx)
}

// ModelGroupTest routes a ping through a group and reports which member answered.
func ModelGroupTest(ctx *context.Context) {
	id := ctx.PathParamInt64("id")
	result, err := agent_service.TestGroup(ctx, id)
	if err != nil {
		if errors.Is(err, agent_model.ErrModelGroupNotFound{}) {
			ctx.NotFound(err)
			return
		}
		ctx.Flash.Error(ctx.Tr("agent.group_test_failed", err.Error()))
		redirectToGroups(ctx)
		return
	}
	failed := 0
	for _, attempt := range result.Attempts {
		if attempt.Error != "" {
			failed++
		}
	}
	log.Trace("agent role %d answered by %s after %d failed attempts", id, result.Model, failed)
	if failed > 0 {
		ctx.Flash.Success(ctx.Tr("agent.group_test_success_fallback", result.Model, failed))
	} else {
		ctx.Flash.Success(ctx.Tr("agent.group_test_success", result.Model))
	}
	redirectToGroups(ctx)
}

// groupForm holds the submitted values so a failed submit can be re-rendered.
// Members keep their raw row values, which is what the select options use.
type groupForm struct {
	Name        string
	Description string
	Mode        string
	Members     []string
}

// memberTargets parses the member rows, reporting the first invalid one.
func (f groupForm) memberTargets() ([]agent_service.GroupMemberTarget, error) {
	targets := make([]agent_service.GroupMemberTarget, 0, len(f.Members))
	for _, value := range f.Members {
		target, err := ParseMemberTarget(value)
		if err != nil {
			return nil, ErrInvalidMember{Value: value}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// ErrInvalidMember reports a member row that could not be understood.
type ErrInvalidMember struct{ Value string }

func (err ErrInvalidMember) Error() string {
	return fmt.Sprintf("invalid member %q", err.Value)
}

// groupMemberValues converts stored members into form member values.
func groupMemberValues(members []*agent_model.ModelGroupMember) []string {
	values := make([]string, 0, len(members))
	for _, m := range members {
		values = append(values, GroupMemberValue(agent_service.GroupMemberTarget{
			ProviderID: m.ProviderID,
			Model:      m.Model,
			GroupID:    m.MemberGroupID,
		}))
	}
	return values
}

// ParseMemberTarget reads a member row value: "model:<providerID>/<model>" or "role:<id>".
func ParseMemberTarget(value string) (agent_service.GroupMemberTarget, error) {
	kind, rest, found := strings.Cut(strings.TrimSpace(value), ":")
	if !found {
		return agent_service.GroupMemberTarget{}, errors.New("invalid member")
	}
	switch kind {
	case agent_model.MemberKindGroup:
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || id == 0 {
			return agent_service.GroupMemberTarget{}, errors.New("invalid role member")
		}
		return agent_service.GroupMemberTarget{GroupID: id}, nil
	case agent_model.MemberKindModel:
		providerPart, model, found := strings.Cut(rest, "/")
		if !found || model == "" {
			return agent_service.GroupMemberTarget{}, errors.New("invalid model member")
		}
		id, err := strconv.ParseInt(providerPart, 10, 64)
		if err != nil || id == 0 {
			return agent_service.GroupMemberTarget{}, errors.New("invalid model member")
		}
		return agent_service.GroupMemberTarget{ProviderID: id, Model: model}, nil
	default:
		return agent_service.GroupMemberTarget{}, errors.New("invalid member")
	}
}

func groupFormFrom(ctx *context.Context) groupForm {
	form := groupForm{
		Name:        ctx.FormTrim("name"),
		Description: ctx.FormTrim("description"),
		Mode:        ctx.FormTrim("mode"),
	}
	form.Members = ctx.FormStrings("member")
	return form
}

// renderGroupForm renders the create form when role is nil, the edit form otherwise.
func renderGroupForm(ctx *context.Context, role *agent_model.ModelGroup, form groupForm, errMsg string) {
	isEdit := role != nil

	ctx.Data["Title"] = ctx.Tr("agent.group_new")
	ctx.Data["PageIsAgentModelGroups"] = true
	if isEdit {
		ctx.Data["Title"] = ctx.Tr("agent.group_edit")
	}
	ctx.Data["Group"] = role
	ctx.Data["IsEdit"] = isEdit
	ctx.Data["Form"] = form
	ctx.Data["ErrorMsg"] = errMsg
	ctx.Data["GroupMembers"] = groupMemberCatalogue(ctx, role)
	ctx.Data["FormAction"] = setting.AppSubURL + groupsURL + "/new"
	if isEdit {
		ctx.Data["FormAction"] = setting.AppSubURL + groupsURL + "/" + strconv.FormatInt(role.ID, 10) + "/edit"
	}
	ctx.Data["CancelLink"] = setting.AppSubURL + groupsURL
	ctx.HTML(http.StatusOK, tplGroupForm)
}

// groupMemberCatalogue lists every target a group can hold: single models of every
// provider, then the other roles.
func groupMemberCatalogue(ctx *context.Context, editing *agent_model.ModelGroup) []groupMemberOption {
	options := make([]groupMemberOption, 0)

	providers, err := agent_service.GetProviders(ctx, "")
	if err == nil {
		for _, p := range providers {
			for _, model := range p.ModelIDs() {
				options = append(options, groupMemberOption{
					Value: fmt.Sprintf("%s:%d/%s", agent_model.MemberKindModel, p.ID, model),
					Label: p.Name + " / " + model,
				})
			}
		}
	}

	roles, _, err := agent_service.FindModelGroups(ctx, agent_model.FindModelGroupsOptions{ListOptions: db.ListOptionsAll})
	if err == nil {
		for _, r := range roles {
			if editing != nil && r.ID == editing.ID {
				continue // a group can not contain itself
			}
			options = append(options, groupMemberOption{
				Value: fmt.Sprintf("%s:%d", agent_model.MemberKindGroup, r.ID),
				Label: "[" + ctx.Locale.TrString("agent.groups_title") + "] " + r.Name,
			})
		}
	}
	return options
}

// groupMemberOption is one selectable member of a group.
type groupMemberOption struct {
	Value string
	Label string
}

// GroupMemberValue renders a stored member as a member row value.
func GroupMemberValue(member agent_service.GroupMemberTarget) string {
	if member.GroupID != 0 {
		return fmt.Sprintf("%s:%d", agent_model.MemberKindGroup, member.GroupID)
	}
	return fmt.Sprintf("%s:%d/%s", agent_model.MemberKindModel, member.ProviderID, member.Model)
}

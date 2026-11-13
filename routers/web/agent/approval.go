// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"net/http"
	"strconv"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

const tplAgentApproval = "agent/approval"

// Approval shows one call that waits for its reader: the page a thread links to, so a reader
// decides where the work is instead of finding the session it belongs to.
func Approval(ctx *context.Context) {
	approval := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(ctx.PathParamInt64("aid")).Get(approval)
	if err != nil {
		ctx.ServerError("get tool approval", err)
		return
	}
	if !has {
		ctx.NotFound(errors.New("no such approval"))
		return
	}
	run, hasRun, err := db.GetByID[agent_model.Run](ctx, approval.RunID)
	if err != nil {
		ctx.ServerError("get run", err)
		return
	}
	// only the person whose question started the run answers for it
	if !hasRun || (run.UserID != 0 && run.UserID != ctx.Doer.ID) {
		ctx.NotFound(errors.New("no such approval"))
		return
	}

	ctx.Data["Title"] = ctx.Tr("agent.approval_title", approval.Tool)
	ctx.Data["Approval"] = approval
	ctx.Data["Pending"] = approval.Status == agent_model.ToolApprovalPending
	session := setting.AppSubURL + "/-/agent/" + strconv.FormatInt(approval.ConversationID, 10)
	ctx.Data["SessionURL"] = session
	ctx.Data["AllowURL"] = session + "/approvals/" + ctx.PathParam("aid") + "/allow"
	ctx.Data["RefuseURL"] = session + "/approvals/" + ctx.PathParam("aid") + "/refuse"
	ctx.Data["AnswerURL"] = session + "/approvals/" + ctx.PathParam("aid") + "/answer"
	question, options, why := agent_service.QuestionOf(approval)
	ctx.Data["Question"], ctx.Data["Options"], ctx.Data["Why"] = question, options, why
	ctx.Data["IsQuestion"] = approval.IsQuestion()
	ctx.HTML(http.StatusOK, tplAgentApproval)
}

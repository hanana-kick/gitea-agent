// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"strconv"

	issues_model "gitea.dev/models/issues"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

// prepareAgentRequest puts the question or approval the agent waits on in front of a reader of
// the thread, so it can be answered where it was asked. Nobody else is told about it: a request
// that is not the reader's to answer is not theirs to see.
func prepareAgentRequest(ctx *context.Context, issue *issues_model.Issue) {
	if setting.Agent.Username == "" || ctx.Doer == nil || !ctx.IsSigned {
		return
	}
	thread, waiting, err := agent_service.PendingThreadRequest(ctx, issue.RepoID, issue.ID)
	if err != nil {
		ctx.ServerError("PendingThreadRequest", err)
		return
	}
	if !waiting {
		return
	}
	may, err := thread.AnswerableBy(ctx, ctx.Doer)
	if err != nil {
		ctx.ServerError("AnswerableBy", err)
		return
	}
	if !may {
		return
	}
	base := setting.AppSubURL + "/-/agent/" + strconv.FormatInt(thread.Run.ConversationID, 10) + "/approvals/" + strconv.FormatInt(thread.Request.ID, 10)
	ctx.Data["AgentRequest"] = thread
	ctx.Data["AgentRequestAllowURL"] = base + "/allow"
	ctx.Data["AgentRequestRefuseURL"] = base + "/refuse"
	ctx.Data["AgentRequestAnswerURL"] = base + "/answer"
	ctx.Data["AgentRequestSessionURL"] = setting.AppSubURL + "/-/agent/" + strconv.FormatInt(thread.Run.ConversationID, 10)
	ctx.Data["AgentRequestBack"] = issue.Link()
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	stdctx "context"
	"net/http"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/timeutil"
	"gitea.dev/services/context"
)

const tplAgentRuns = "agent/runs"

// agentRunRow is one run as the monitoring page reads it.
type agentRunRow struct {
	ID           int64
	Kind         string
	Repository   string
	User         string
	Status       string
	Error        string
	Model        string
	WaitingTool  string // the call a waiting run waits for
	SessionID    int64
	Created      timeutil.TimeStamp
	Started      timeutil.TimeStamp
	Finished     timeutil.TimeStamp
	DurationText string
}

// Runs lists the agent's runs, newest first: the page that says whether the agent is working,
// what it did, and why it stopped.
func Runs(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("agent.runs_title")
	ctx.Data["PageIsAdminAgentRuns"] = true

	status := ctx.FormTrim("status")
	page := max(ctx.FormInt("page"), 1)
	runs := make([]*agent_model.Run, 0, 50)
	sess := db.GetEngine(ctx).OrderBy("id DESC")
	if status != "" {
		sess = sess.Where("status = ?", status)
	}
	count, err := sess.Limit(50, (page-1)*50).FindAndCount(&runs)
	if err != nil {
		ctx.ServerError("find runs", err)
		return
	}

	rows := make([]*agentRunRow, 0, len(runs))
	conversationIDs := make([]int64, 0, len(runs))
	userIDs := make([]int64, 0, len(runs))
	for _, run := range runs {
		conversationIDs = append(conversationIDs, run.ConversationID)
		userIDs = append(userIDs, run.UserID)
	}
	conversations, err := conversationsByID(ctx, conversationIDs)
	if err != nil {
		ctx.ServerError("find conversations", err)
		return
	}
	users, err := usersByID(ctx, userIDs)
	if err != nil {
		ctx.ServerError("find users", err)
		return
	}
	repos, err := repositoriesOf(ctx, conversations)
	if err != nil {
		ctx.ServerError("find repositories", err)
		return
	}
	waiting, err := waitingCalls(ctx, runs)
	if err != nil {
		ctx.ServerError("find waiting calls", err)
		return
	}
	for _, run := range runs {
		session := conversations[run.ConversationID]
		row := &agentRunRow{
			ID: run.ID, Status: run.Status, Error: run.Error, Model: run.Model,
			Created: run.CreatedUnix, Started: run.StartedUnix, Finished: run.FinishedUnix,
			SessionID: run.ConversationID, Kind: agent_model.ConversationKindServer,
		}
		if session != nil {
			row.Kind = session.Kind
			row.Repository = repos[session.RepoID]
		}
		if user := users[run.UserID]; user != nil {
			row.User = user.Name
		}
		if call, ok := waiting[run.ID]; ok {
			row.WaitingTool = call
		}
		if run.StartedUnix > 0 && run.FinishedUnix > 0 {
			row.DurationText = (run.FinishedUnix - run.StartedUnix).AsTime().String()
		}
		rows = append(rows, row)
	}

	ctx.Data["Runs"] = rows
	ctx.Data["Status"] = status
	ctx.Data["Page"] = page
	ctx.Data["Total"] = count
	ctx.Data["Running"] = countOf(rows, agent_model.RunStatusRunning)
	ctx.Data["Waiting"] = countOf(rows, agent_model.RunStatusWaiting)
	ctx.Data["Failed"] = countOf(rows, agent_model.RunStatusFailed)
	ctx.HTML(http.StatusOK, tplAgentRuns)
}

func countOf(rows []*agentRunRow, status string) int {
	shown := 0
	for _, row := range rows {
		if row.Status == status {
			shown++
		}
	}
	return shown
}

func conversationsByID(ctx stdctx.Context, ids []int64) (map[int64]*agent_model.Conversation, error) {
	found := make(map[int64]*agent_model.Conversation, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	list := make([]*agent_model.Conversation, 0, len(ids))
	if err := db.GetEngine(ctx).In("id", ids).Find(&list); err != nil {
		return nil, err
	}
	for _, session := range list {
		found[session.ID] = session
	}
	return found, nil
}

func usersByID(ctx stdctx.Context, ids []int64) (map[int64]*user_model.User, error) {
	found := make(map[int64]*user_model.User, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	list := make([]*user_model.User, 0, len(ids))
	if err := db.GetEngine(ctx).In("id", ids).Find(&list); err != nil {
		return nil, err
	}
	for _, user := range list {
		found[user.ID] = user
	}
	return found, nil
}

// repositoriesOf names the repositories of the sessions, so a run reads as where it worked.
func repositoriesOf(ctx stdctx.Context, conversations map[int64]*agent_model.Conversation) (map[int64]string, error) {
	found := make(map[int64]string, len(conversations))
	ids := make([]int64, 0, len(conversations))
	seen := make(map[int64]bool, len(conversations))
	for _, session := range conversations {
		if session.RepoID == 0 || seen[session.RepoID] {
			continue
		}
		seen[session.RepoID] = true
		ids = append(ids, session.RepoID)
	}
	if len(ids) == 0 {
		return found, nil
	}
	repos, err := repo_model.GetRepositoriesMapByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, repo := range repos {
		found[id] = repo.FullName()
	}
	return found, nil
}

// waitingCalls names the call each waiting run waits for.
func waitingCalls(ctx stdctx.Context, runs []*agent_model.Run) (map[int64]string, error) {
	found := make(map[int64]string, len(runs))
	ids := make([]int64, 0, len(runs))
	for _, run := range runs {
		if run.Status == agent_model.RunStatusWaiting {
			ids = append(ids, run.ID)
		}
	}
	if len(ids) == 0 {
		return found, nil
	}
	approvals := make([]*agent_model.ToolApproval, 0, len(ids))
	if err := db.GetEngine(ctx).In("run_id", ids).And("status = ?", agent_model.ToolApprovalPending).Find(&approvals); err != nil {
		return nil, err
	}
	for _, approval := range approvals {
		found[approval.RunID] = approval.Tool
	}
	return found, nil
}

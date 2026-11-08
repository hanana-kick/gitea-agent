// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	stdctx "context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
	agent_service "gitea.dev/services/agent"
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
	QueuePlace   string // where a run waiting for a worker stands
	QueueWait    string // how long a waiting run has waited
	Active       bool   // whether the run can still be stopped
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
		row.Active = agent_model.IsRunActive(run.Status)
		if state := agent_service.RunStateOf(run); state.QueuePosition > 0 {
			row.QueuePlace = fmt.Sprintf("%d/%d", state.QueuePosition, state.QueueWaiting)
			row.QueueWait = queueWaitedText(ctx, state.QueueWaitedSeconds)
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
	labels := map[string]string{
		"queued": agent_model.RunStatusQueued, "waiting": agent_model.RunStatusWaiting,
		"running": agent_model.RunStatusRunning, "succeeded": agent_model.RunStatusSucceeded,
		"failed": agent_model.RunStatusFailed, "canceled": agent_model.RunStatusCanceled,
	}
	for status, key := range map[string]string{
		agent_model.RunStatusQueued:    "agent.runs_status_queued",
		agent_model.RunStatusWaiting:   "agent.runs_status_waiting",
		agent_model.RunStatusRunning:   "agent.runs_status_running",
		agent_model.RunStatusSucceeded: "agent.runs_status_succeeded",
		agent_model.RunStatusFailed:    "agent.runs_status_failed",
		agent_model.RunStatusCanceled:  "agent.runs_status_canceled",
	} {
		labels[status] = string(ctx.Tr(key))
	}
	// the queue words are shared with the chat, so a waiting run is described the same way twice
	labels["queuedAt"] = string(ctx.Tr("agent.chat_queue_position"))
	labels["queuedWaitedSeconds"] = string(ctx.Tr("agent.chat_queue_waited_seconds"))
	labels["queuedWaitedMinutes"] = string(ctx.Tr("agent.chat_queue_waited_minutes"))
	encoded, err := json.Marshal(labels)
	if err != nil {
		ctx.ServerError("labels", err)
		return
	}
	ctx.Data["LabelsJSON"] = string(encoded)
	ctx.Data["RunsEventsURL"] = setting.AppSubURL + "/-/admin/agent/runs/events"
	ctx.HTML(http.StatusOK, tplAgentRuns)
}

// RunTrace answers what one run handed its model, as it was written: the prompt, the tools it
// was offered and the messages it sent. It is only there when the instance traces.
func RunTrace(ctx *context.Context) {
	body, err := agent_service.ReadPromptTrace(ctx.PathParamInt64("id"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			ctx.NotFound(err)
			return
		}
		ctx.ServerError("read the prompt trace", err)
		return
	}
	ctx.Resp.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = ctx.Resp.Write(body)
}

// RunStop stops one run of the agent: a run that waits in the queue or for a reader is finished
// here, and one that is answering is told to stop.
func RunStop(ctx *context.Context) {
	run, exist, err := db.GetByID[agent_model.Run](ctx, ctx.PathParamInt64("id"))
	if err != nil {
		ctx.ServerError("find run", err)
		return
	}
	if !exist {
		ctx.NotFound(errors.New("no such run"))
		return
	}
	if err := agent_service.CancelRun(run.ConversationID); err != nil {
		if !errors.Is(err, agent_service.ErrNoActiveRun{}) {
			ctx.ServerError("stop run", err)
			return
		}
		ctx.Flash.Info(ctx.Tr("agent.runs_already_over"))
	} else {
		ctx.Flash.Success(ctx.Tr("agent.runs_stopped"))
	}
	ctx.Redirect(setting.AppSubURL + "/-/admin/agent/runs")
}

// RunsEvents streams every run change to the monitoring page, so the table follows the agent
// without a reload.
func RunsEvents(ctx *context.Context) {
	flusher, ok := ctx.Resp.(http.Flusher)
	if !ok {
		ctx.ServerError("agent runs events", errors.New("the response writer does not support streaming"))
		return
	}
	header := ctx.Resp.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	ctx.Resp.WriteHeader(http.StatusOK)
	flusher.Flush()

	runs, unsubscribe := agent_service.SubscribeAllRunEvents()
	defer unsubscribe()

	// what is still going is sent first, so a page that connects mid-queue shows where every run
	// stands rather than waiting for the next change
	if states, err := agent_service.ActiveRunStates(ctx); err == nil {
		for _, state := range states {
			payload, err := json.Marshal(state)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(ctx.Resp, "data: %s\n\n", payload); err != nil {
				return
			}
		}
		flusher.Flush()
	}

	heartbeat := time.NewTicker(eventsHeartbeat)
	defer heartbeat.Stop()

	clientGone := ctx.Req.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(ctx.Resp, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-runs:
			if !ok {
				return
			}
			if event.Name != agent_service.EventRun {
				continue
			}
			payload, err := json.Marshal(event.Data)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(ctx.Resp, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// queueWaitedText is how long a run has waited, for the runs table.
func queueWaitedText(ctx *context.Context, seconds int) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds < 90:
		return fmt.Sprintf(string(ctx.Tr("agent.chat_queue_waited_seconds")), seconds)
	default:
		return fmt.Sprintf(string(ctx.Tr("agent.chat_queue_waited_minutes")), seconds/60)
	}
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

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
)

// ThreadRequest is a request the runs of one issue or pull request wait on: a call that needs its
// reader's permission, or a question the work can not go on without.
type ThreadRequest struct {
	Request *agent_model.ToolApproval
	Run     *agent_model.Run
	// Question is what was asked, and Options the answers offered, for a question.
	Question string
	Options  []string
	Why      string
}

// PendingThreadRequest returns the newest request the runs reporting into a thread wait on, so a
// reader of that thread can answer it where it was asked, and whether there is one.
func PendingThreadRequest(ctx context.Context, repoID, issueID int64) (*ThreadRequest, bool, error) {
	if repoID == 0 || issueID == 0 {
		return nil, false, nil
	}
	request := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).Table(&agent_model.ToolApproval{}).
		Join("INNER", "agent_run", "agent_run.id = agent_tool_approval.run_id").
		Where("agent_tool_approval.status = ?", agent_model.ToolApprovalPending).
		And("agent_run.reply_repo_id = ?", repoID).
		And("agent_run.reply_issue_id = ?", issueID).
		OrderBy("agent_tool_approval.id DESC").Get(request)
	if err != nil {
		return nil, false, fmt.Errorf("find request of repo %d issue %d: %w", repoID, issueID, err)
	}
	if !has {
		return nil, false, nil
	}
	run, exist, err := db.GetByID[agent_model.Run](ctx, request.RunID)
	if err != nil {
		return nil, false, err
	}
	if !exist {
		return nil, false, nil
	}
	thread := &ThreadRequest{Request: request, Run: run}
	thread.Question, thread.Options, thread.Why = QuestionOf(request)
	return thread, true, nil
}

// QuestionOf reads what a request asks: the question, the answers offered, and what the answer
// changes. A request that is not a question asks for a call, and its tool is what it is about.
func QuestionOf(request *agent_model.ToolApproval) (question string, options []string, why string) {
	if !request.IsQuestion() {
		return request.Tool, nil, ""
	}
	var params struct {
		Question string `json:"question"`
		Options  string `json:"options"`
		Why      string `json:"why"`
	}
	if err := json.Unmarshal([]byte(request.Arguments), &params); err != nil {
		return request.Tool, nil, ""
	}
	question, why = strings.TrimSpace(params.Question), strings.TrimSpace(params.Why)
	for option := range strings.SplitSeq(params.Options, "|") {
		if option = strings.TrimSpace(option); option != "" {
			options = append(options, option)
		}
	}
	if question == "" {
		question = request.Tool
	}
	return question, options, why
}

// AnswerableBy reports whether a reader may answer the request in the thread.
func (t *ThreadRequest) AnswerableBy(ctx context.Context, doer *user_model.User) (bool, error) {
	return CanAnswerRun(ctx, doer, t.Run)
}

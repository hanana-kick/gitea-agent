// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

import "time"

// AgentSession is one session of the agent: what it works on, whether it decides for itself, and
// what its newest run is doing.
// swagger:model
type AgentSession struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	// Title is what the session is called, and Summary what its earlier work came to.
	Title      string    `json:"title"`
	Summary    string    `json:"summary,omitempty"`
	RepoID     int64     `json:"repo_id,omitempty"`
	Repository string    `json:"repository,omitempty"`
	Autopilot  bool      `json:"autopilot"`
	Icon       string    `json:"icon,omitempty"`
	FolderID   int64     `json:"folder_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Run is the newest run of the session, which is what a client watches.
	Run *AgentRun `json:"run,omitempty"`
	// PendingApprovals is how many calls wait for a reader right now.
	PendingApprovals int64 `json:"pending_approvals"`
}

// AgentRun is one run of a session: the state the engine reports, and what the model answered.
// swagger:model
type AgentRun struct {
	ID int64 `json:"id"`
	// Status is queued, running, waiting, succeeded, failed or canceled.
	Status     string `json:"status"`
	Model      string `json:"model,omitempty"`
	Error      string `json:"error,omitempty"`
	Cancelable bool   `json:"cancelable"`
	// QueuePosition is where a run waiting for a worker stands, counting from one.
	QueuePosition int `json:"queue_position,omitempty"`
	QueueWaiting  int `json:"queue_waiting,omitempty"`
	// WaitingTool is the call a waiting run waits for.
	WaitingTool string    `json:"waiting_tool,omitempty"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
}

// AgentMessage is one message of a transcript.
// swagger:model
type AgentMessage struct {
	ID   int64  `json:"id"`
	Role string `json:"role"`
	// Reasoning is what the model thought before it wrote the message; only an answer has it.
	Content   string    `json:"content"`
	Reasoning string    `json:"reasoning,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	EditedAt  time.Time `json:"edited_at,omitzero"`
}

// AgentApproval is a call that waits for a reader: permission for a call, or a question the run
// can not go on without.
// swagger:model
type AgentApproval struct {
	ID   int64  `json:"id"`
	Tool string `json:"tool"`
	// Kind is approval or question.
	Kind string `json:"kind"`
	// Arguments is what the call was made with, as the model wrote it.
	Arguments string    `json:"arguments"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentSessionList is the sessions a client may open.
type AgentSessionList struct {
	List  []*AgentSession `json:"list"`
	Total int64           `json:"total"`
}

// AgentMessageList is one page of a transcript.
type AgentMessageList struct {
	List []*AgentMessage `json:"list"`
	// HasMore says whether older messages are left before the first one here.
	HasMore bool `json:"has_more"`
}

// AgentApprovalList is what a session waits for.
type AgentApprovalList struct {
	List  []*AgentApproval `json:"list"`
	Total int64            `json:"total"`
}

// CreateAgentSessionOption is the body of a request that opens a session.
// swagger:model
type CreateAgentSessionOption struct {
	// Kind is what the session works on: repository or server.
	Kind string `json:"kind" binding:"Required"`
	// RepoID names the repository of a repository session.
	RepoID int64 `json:"repo_id"`
	// Content is the first message, which starts a run when it is given.
	Content string `json:"content"`
	// Autopilot lets this session decide for itself instead of asking a reader.
	Autopilot *bool `json:"autopilot"`
}

// PostAgentMessageOption is the body of a request that sends a message.
// swagger:model
type PostAgentMessageOption struct {
	Content string `json:"content" binding:"Required"`
}

// AgentAutopilotOption is the body of a request that sets who decides.
// swagger:model
type AgentAutopilotOption struct {
	Autopilot bool `json:"autopilot"`
}

// AnswerAgentApprovalOption is the body of a request that answers a call: allow or refuse it, or
// answer the question it holds.
// swagger:model
type AnswerAgentApprovalOption struct {
	// Decision is allow or refuse.
	Decision string `json:"decision" binding:"Required"`
	// Answer is what a question is answered with, instead of a decision.
	Answer string `json:"answer"`
}

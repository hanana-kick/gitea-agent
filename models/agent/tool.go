// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// ToolPermission holds the admin-chosen mode of one agent tool.
type ToolPermission struct {
	ID   int64  `xorm:"pk autoincr"`
	Tool string `xorm:"UNIQUE NOT NULL"`
	Mode string `xorm:"NOT NULL DEFAULT 'llm'"` // llm, manual, allow_all
	// Enabled gates the tool entirely: a disabled tool is not offered at all.
	Enabled bool `xorm:"NOT NULL DEFAULT TRUE"`
}

func (ToolPermission) TableName() string {
	return "agent_tool_permission"
}

// ToolApproval statuses: a manual tool call waits here until its user decides.
// Kinds of request a run can leave for its reader.
const (
	// ToolApprovalKindApproval is a call the reader allows or refuses.
	ToolApprovalKindApproval = "approval"
	// ToolApprovalKindQuestion is a question the run waits for an answer to.
	ToolApprovalKindQuestion = "question"
)

const (
	ToolApprovalPending = "pending"
	ToolApprovalAllowed = "allowed"
	ToolApprovalRefused = "refused"
	ToolApprovalExpired = "expired"
)

// ToolApproval is one tool call the agent may only make once a user has allowed it.
type ToolApproval struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	RunID          int64  `xorm:"INDEX NOT NULL"`
	Tool           string `xorm:"VARCHAR(160) NOT NULL"`
	Arguments      string `xorm:"TEXT NOT NULL"`
	// Kind says what is being asked: a call that needs its reader's permission, or a question the
	// run can not go on without. Both wait the same way and are answered in the same place.
	Kind string `xorm:"VARCHAR(16) NOT NULL DEFAULT 'approval'"`
	// Answer is what a reader wrote back, for the questions that take words rather than a yes.
	Answer string `xorm:"TEXT"`
	// Result is what the call returned, kept until the run that asked for it reads it back.
	Result string `xorm:"TEXT"`
	Status string `xorm:"VARCHAR(16) NOT NULL"`
	// DecidedUserID is the user who answered, so a shared session cannot decide twice.
	DecidedUserID int64              `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix   timeutil.TimeStamp `xorm:"created NOT NULL"`
	DecidedUnix   timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (ToolApproval) TableName() string {
	return "agent_tool_approval"
}

// IsQuestion reports whether the request waits for words rather than for a yes or a no.
func (a *ToolApproval) IsQuestion() bool {
	return a.Kind == ToolApprovalKindQuestion
}

// KindOrDefault is the kind of a row written before kinds were stored, which was an approval.
func (a *ToolApproval) KindOrDefault() string {
	if a.Kind == "" {
		return ToolApprovalKindApproval
	}
	return a.Kind
}

func init() {
	db.RegisterModel(new(ToolPermission))
	db.RegisterModel(new(ToolApproval))
}

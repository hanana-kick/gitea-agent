// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentToolApprovalV375 is the frozen shape of agent_tool_approval, with what the call returned,
// kept until the run that asked for it reads it back.
type agentRunV375 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	UserID         int64  `xorm:"NOT NULL DEFAULT 0"`
	Status         string `xorm:"VARCHAR(16) NOT NULL"`
	Model          string `xorm:"VARCHAR(160) NOT NULL DEFAULT ''"`
	Error          string `xorm:"TEXT"`

	ReplyRepoID     int64 `xorm:"NOT NULL DEFAULT 0"`
	ReplyIssueID    int64 `xorm:"NOT NULL DEFAULT 0"`
	StatusCommentID int64 `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix  timeutil.TimeStamp `xorm:"created NOT NULL"`
	StartedUnix  timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
	FinishedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (agentRunV375) TableName() string {
	return "agent_tool_approval"
}

// AddAgentToolApprovalResult keeps what an allowed call returned, so the run it paused continues
// with it after the answer, however late that answer comes.
func AddAgentToolApprovalResult(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentRunV375))
	return err
}

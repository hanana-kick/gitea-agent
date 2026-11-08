// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentToolApprovalV377 is the frozen shape of agent_tool_approval, with what the call returned,
// kept until the run that asked for it reads it back.
type agentToolApprovalV377 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	RunID          int64  `xorm:"INDEX NOT NULL"`
	Tool           string `xorm:"VARCHAR(160) NOT NULL"`
	Arguments      string `xorm:"TEXT NOT NULL"`
	Status         string `xorm:"VARCHAR(16) NOT NULL"`
	DecidedUserID  int64  `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	DecidedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (agentToolApprovalV377) TableName() string {
	return "agent_tool_approval"
}

// AddAgentToolApprovalResult keeps what an allowed call returned, so the run it paused continues
// with it after the answer, however late that answer comes.
func AddAgentToolApprovalResult(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentToolApprovalV377))
	return err
}

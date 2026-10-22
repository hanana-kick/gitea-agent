// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentToolApprovalV371 is the frozen shape of the table holding tool calls that wait for
// a reader to allow them.
type agentToolApprovalV371 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	RunID          int64  `xorm:"INDEX NOT NULL"`
	Tool           string `xorm:"VARCHAR(160) NOT NULL"`
	Arguments      string `xorm:"TEXT NOT NULL"`
	Status         string `xorm:"VARCHAR(16) NOT NULL"`

	DecidedUserID int64              `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix   timeutil.TimeStamp `xorm:"created NOT NULL"`
	DecidedUnix   timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (agentToolApprovalV371) TableName() string {
	return "agent_tool_approval"
}

// AddAgentToolApproval stores the tool calls waiting for a user to allow them.
func AddAgentToolApproval(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentToolApprovalV371))
	return err
}

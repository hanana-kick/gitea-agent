// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentToolApprovalV379 is the frozen shape of agent_tool_approval, with what a request is and
// what a reader answered it with.
type agentToolApprovalV379 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	RunID          int64  `xorm:"INDEX NOT NULL"`
	Tool           string `xorm:"VARCHAR(160) NOT NULL"`
	Arguments      string `xorm:"TEXT NOT NULL"`
	Result         string `xorm:"TEXT"`
	Status         string `xorm:"VARCHAR(16) NOT NULL"`
	DecidedUserID  int64  `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	DecidedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (agentToolApprovalV379) TableName() string {
	return "agent_tool_approval"
}

// AddAgentRequestKindAndAnswer tells an approval from a question, and keeps what a reader wrote
// back to one, so a run can wait for either and be answered where it was asked.
func AddAgentRequestKindAndAnswer(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentToolApprovalV379),
		new(agentToolApprovalColumnsV379{}))
	return err
}

// agentToolApprovalColumnsV379 carries the two columns this migration adds; the frozen shape
// above is the table as it was.
type agentToolApprovalColumnsV379 struct {
	ID     int64  `xorm:"pk autoincr"`
	Kind   string `xorm:"VARCHAR(16) NOT NULL DEFAULT 'approval'"`
	Answer string `xorm:"TEXT"`
}

func (agentToolApprovalColumnsV379) TableName() string {
	return "agent_tool_approval"
}

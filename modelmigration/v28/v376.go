// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentRunV376 is the frozen shape of agent_run, with the thread a run reports into and the
// comment it says how it is going in.
type agentRunV376 struct {
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

func (agentRunV376) TableName() string {
	return "agent_run"
}

// AddAgentRunStatusComment records where a run says how it is going: the thread it answers in,
// and the comment a reader watches.
func AddAgentRunStatusComment(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentRunV376))
	return err
}

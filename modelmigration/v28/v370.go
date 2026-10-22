// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentRunV370 is the frozen shape of agent_run when the asking user was added.
type agentRunV370 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	UserID         int64  `xorm:"NOT NULL DEFAULT 0"`
	Status         string `xorm:"VARCHAR(16) NOT NULL"`
	Model          string `xorm:"VARCHAR(160) NOT NULL DEFAULT ''"`
	Error          string `xorm:"TEXT"`

	CreatedUnix  timeutil.TimeStamp `xorm:"created NOT NULL"`
	StartedUnix  timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
	FinishedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (agentRunV370) TableName() string {
	return "agent_run"
}

// AddAgentRunUser records who asked for a run, so its tools run with that user's rights.
func AddAgentRunUser(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentRunV370))
	return err
}

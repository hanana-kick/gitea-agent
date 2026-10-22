// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentMessageV369 is the frozen shape of agent_message with the mark of the messages a
// user rewrote.
type agentMessageV369 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	Role           string `xorm:"VARCHAR(16) NOT NULL"`
	Content        string `xorm:"TEXT NOT NULL"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	EditedUnix  timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (agentMessageV369) TableName() string {
	return "agent_message"
}

// AddAgentMessageEdited marks the messages a user rewrote.
func AddAgentMessageEdited(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentMessageV369))
	return err
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentMessageV373 is the frozen shape of the transcript table, with the reasoning a model
// wrote before its answer.
type agentMessageV373 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	Role           string `xorm:"VARCHAR(16) NOT NULL"`
	Content        string `xorm:"TEXT NOT NULL"`
	Reasoning      string `xorm:"TEXT"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	EditedUnix  timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (agentMessageV373) TableName() string {
	return "agent_message"
}

// AddAgentMessageReasoning stores the reasoning of an answer in the transcript, so a reader
// can open it again after the run is over.
func AddAgentMessageReasoning(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentMessageV373))
	return err
}

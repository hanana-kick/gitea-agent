// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentRunV360 is the frozen shape of agent_run, one server-side agent execution.
type agentRunV360 struct {
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

func (agentRunV360) TableName() string {
	return "agent_run"
}

// agentConversationV360 is the frozen shape of agent_conversation with the model
// binding column of a session.
type agentConversationV360 struct {
	ID      int64  `xorm:"pk autoincr"`
	Kind    string `xorm:"VARCHAR(16) NOT NULL"`
	OwnerID int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	RepoID  int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	Title   string `xorm:"VARCHAR(100) NOT NULL"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentConversationV360) TableName() string {
	return "agent_conversation"
}

// AddAgentRunTable creates the agent run table and adds the model binding column of a session.
func AddAgentRunTable(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentRunV360), new(agentConversationV360))
	return err
}

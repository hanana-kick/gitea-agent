// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentConversationV361 is the frozen shape of agent_conversation, one agent chat session.
type agentConversationV361 struct {
	ID      int64  `xorm:"pk autoincr"`
	Kind    string `xorm:"VARCHAR(16) NOT NULL"`
	OwnerID int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	RepoID  int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	Title   string `xorm:"VARCHAR(100) NOT NULL"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentConversationV361) TableName() string {
	return "agent_conversation"
}

// agentMessageV361 is the frozen shape of agent_message, one transcript entry.
type agentMessageV361 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	Role           string `xorm:"VARCHAR(16) NOT NULL"`
	Content        string `xorm:"TEXT NOT NULL"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
}

func (agentMessageV361) TableName() string {
	return "agent_message"
}

// AddAgentConversationTables creates the session and transcript tables of the agent.
func AddAgentConversationTables(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentConversationV361), new(agentMessageV361))
	return err
}

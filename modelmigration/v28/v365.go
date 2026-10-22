// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentConversationV365 is the frozen shape of agent_conversation with the recent work
// summary of a session.
type agentConversationV365 struct {
	ID      int64  `xorm:"pk autoincr"`
	Kind    string `xorm:"VARCHAR(16) NOT NULL"`
	OwnerID int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	RepoID  int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	Title   string `xorm:"VARCHAR(100) NOT NULL"`

	Summary  string `xorm:"VARCHAR(200) NOT NULL DEFAULT ''"`
	Icon     string `xorm:"VARCHAR(60) NOT NULL DEFAULT ''"`
	TitleSet bool   `xorm:"NOT NULL DEFAULT FALSE"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentConversationV365) TableName() string {
	return "agent_conversation"
}

// AddAgentConversationSummary adds the recent work summary of a session.
func AddAgentConversationSummary(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentConversationV365))
	return err
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentConversationV362 is the frozen shape of agent_conversation with the naming state
// and the icon of a session.
type agentConversationV362 struct {
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

func (agentConversationV362) TableName() string {
	return "agent_conversation"
}

// AddAgentConversationTitleAndIcon adds the naming state and the icon of a session.
func AddAgentConversationTitleAndIcon(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentConversationV362))
	return err
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentConversationV368 is the frozen shape of agent_conversation with its owner.
type agentConversationV368 struct {
	ID      int64  `xorm:"pk autoincr"`
	Kind    string `xorm:"VARCHAR(16) NOT NULL"`
	OwnerID int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	RepoID  int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	Title   string `xorm:"VARCHAR(100) NOT NULL"`

	Summary  string `xorm:"VARCHAR(200) NOT NULL DEFAULT ''"`
	Icon     string `xorm:"VARCHAR(60) NOT NULL DEFAULT ''"`
	FolderID int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	TitleSet bool   `xorm:"NOT NULL DEFAULT FALSE"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentConversationV368) TableName() string {
	return "agent_conversation"
}

// AddAgentConversationOwner gives every agent session an owner.
func AddAgentConversationOwner(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentConversationV368))
	return err
}

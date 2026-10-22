// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentConversationFolderV363 is the frozen shape of agent_conversation_folder, a group
// of conversations in the sidebar.
type agentConversationFolderV363 struct {
	ID       int64  `xorm:"pk autoincr"`
	Name     string `xorm:"VARCHAR(100) NOT NULL"`
	Icon     string `xorm:"VARCHAR(60) NOT NULL DEFAULT ''"`
	Position int    `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentConversationFolderV363) TableName() string {
	return "agent_conversation_folder"
}

// agentConversationV363 is the frozen shape of agent_conversation with the folder column.
type agentConversationV363 struct {
	ID       int64  `xorm:"pk autoincr"`
	Kind     string `xorm:"VARCHAR(16) NOT NULL"`
	OwnerID  int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	RepoID   int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	Title    string `xorm:"VARCHAR(100) NOT NULL"`
	Summary  string `xorm:"VARCHAR(200) NOT NULL DEFAULT ''"`
	Icon     string `xorm:"VARCHAR(60) NOT NULL DEFAULT ''"`
	FolderID int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	TitleSet bool   `xorm:"NOT NULL DEFAULT FALSE"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentConversationV363) TableName() string {
	return "agent_conversation"
}

// AddAgentConversationFolders creates the folders conversations can be filed under.
func AddAgentConversationFolders(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentConversationFolderV363), new(agentConversationV363))
	return err
}

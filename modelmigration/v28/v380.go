// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentConversationV380 is the frozen shape of agent_conversation, with the flag that lets one
// session decide for itself.
type agentConversationV380 struct {
	ID        int64  `xorm:"pk autoincr"`
	Kind      string `xorm:"VARCHAR(16) NOT NULL"`
	OwnerID   int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	RepoID    int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	Title     string `xorm:"VARCHAR(100) NOT NULL"`
	Autopilot bool   `xorm:"NOT NULL DEFAULT false"`
	Summary   string `xorm:"VARCHAR(200) NOT NULL DEFAULT ''"`
	Icon      string `xorm:"VARCHAR(60) NOT NULL DEFAULT ''"`
	FolderID  int64  `xorm:"INDEX NOT NULL DEFAULT 0"`
	TitleSet  bool   `xorm:"NOT NULL DEFAULT FALSE"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentConversationV380) TableName() string {
	return "agent_conversation"
}

// AddAgentConversationAutopilot lets one session decide for itself, so a run can finish without
// a reader wherever the instance allows it.
func AddAgentConversationAutopilot(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentConversationV380))
	return err
}

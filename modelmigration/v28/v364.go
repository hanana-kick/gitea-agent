// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentAttachmentV364 is the frozen shape of agent_attachment, a file sent with a message.
type agentAttachmentV364 struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	MessageID      int64  `xorm:"INDEX NOT NULL"`
	Name           string `xorm:"VARCHAR(255) NOT NULL"`
	Size           int64  `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
}

func (agentAttachmentV364) TableName() string {
	return "agent_attachment"
}

// AddAgentAttachments creates the table of files sent with a message.
func AddAgentAttachments(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentAttachmentV364))
	return err
}

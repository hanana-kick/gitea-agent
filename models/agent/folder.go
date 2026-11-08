// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// ConversationFolder groups conversations in the sidebar. It only organises them:
// membership changes nothing about how an agent runs.
type ConversationFolder struct {
	ID   int64  `xorm:"pk autoincr"`
	Name string `xorm:"VARCHAR(100) NOT NULL"`
	// Icon is an icon name of the icon set, or empty for no icon.
	Icon     string `xorm:"VARCHAR(60) NOT NULL DEFAULT ''"`
	Position int    `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (ConversationFolder) TableName() string {
	return "agent_conversation_folder"
}

func init() {
	db.RegisterModel(new(ConversationFolder))
}

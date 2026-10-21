// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"gitea.dev/models/db"
)

// ToolPermission holds the admin-chosen mode of one agent tool.
type ToolPermission struct {
	ID   int64  `xorm:"pk autoincr"`
	Tool string `xorm:"UNIQUE NOT NULL"`
	Mode string `xorm:"NOT NULL DEFAULT 'llm'"` // llm, manual, allow_all
	// Enabled gates the tool entirely: a disabled tool is not offered at all.
	Enabled bool `xorm:"NOT NULL DEFAULT TRUE"`
}

func (ToolPermission) TableName() string {
	return "agent_tool_permission"
}

func init() {
	db.RegisterModel(new(ToolPermission))
}

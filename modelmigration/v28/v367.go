// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"

	"xorm.io/xorm"
)

// agentToolPermissionV367 is the frozen shape of agent_tool_permission, the admin-chosen
// mode of one agent tool.
type agentToolPermissionV367 struct {
	ID      int64  `xorm:"pk autoincr"`
	Tool    string `xorm:"UNIQUE NOT NULL"`
	Mode    string `xorm:"NOT NULL DEFAULT 'llm'"`
	Enabled bool   `xorm:"NOT NULL DEFAULT TRUE"`
}

func (agentToolPermissionV367) TableName() string {
	return "agent_tool_permission"
}

// AddAgentToolPermission adds the per-tool permission modes of the agent.
func AddAgentToolPermission(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentToolPermissionV367))
	return err
}

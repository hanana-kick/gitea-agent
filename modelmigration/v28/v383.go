// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentMCPServerV383 is the frozen shape of agent_mcp_server: a Model Context Protocol server
// whose tools join the ones the agent is offered.
type agentMCPServerV383 struct {
	ID          int64  `xorm:"pk autoincr"`
	Name        string `xorm:"UNIQUE NOT NULL"`
	Slug        string `xorm:"UNIQUE NOT NULL"`
	URL         string `xorm:"VARCHAR(255) NOT NULL"`
	Description string `xorm:"TEXT"`
	TokenEnc    string `xorm:"TEXT"`
	ReadOnly    bool   `xorm:"NOT NULL DEFAULT false"`
	Enabled     bool   `xorm:"NOT NULL DEFAULT true"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentMCPServerV383) TableName() string {
	return "agent_mcp_server"
}

// AddAgentMCPServer stores the servers an instance lets its agent call.
func AddAgentMCPServer(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentMCPServerV383))
	return err
}

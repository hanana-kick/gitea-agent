// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentModelRoleV366 is the frozen shape of agent_model_role, a model assigned to one
// role of the agent.
type agentModelRoleV366 struct {
	ID          int64              `xorm:"pk autoincr"`
	Role        string             `xorm:"UNIQUE NOT NULL"`
	ModelRef    string             `xorm:"VARCHAR(160) NOT NULL DEFAULT ''"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentModelRoleV366) TableName() string {
	return "agent_model_role"
}

// AddAgentModelRole adds the models the admin assigns to the agent roles.
func AddAgentModelRole(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentModelRoleV366))
	return err
}

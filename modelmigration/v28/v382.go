// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentModelGroupV382 is the frozen shape of agent_model_group, with the flag that says which
// kind of model the group fronts.
type agentModelGroupV382 struct {
	ID          int64  `xorm:"pk autoincr"`
	Name        string `xorm:"UNIQUE NOT NULL"`
	Slug        string `xorm:"UNIQUE NOT NULL"`
	Description string `xorm:"TEXT"`
	Mode        string `xorm:"NOT NULL"`
	Cursor      int    `xorm:"NOT NULL DEFAULT 0"`
	Decisions   bool   `xorm:"NOT NULL DEFAULT false"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentModelGroupV382) TableName() string {
	return "agent_model_group"
}

// AddAgentModelGroupDecisions lets an operator say which kind of model a group fronts, so a group
// is offered to the roles that ask for that kind. Groups stored before it front what writes.
func AddAgentModelGroupDecisions(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentModelGroupV382))
	return err
}

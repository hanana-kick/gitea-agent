// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentModelGroupV360 is the frozen shape of agent_model_group, a set of routable members.
type agentModelGroupV360 struct {
	ID          int64  `xorm:"pk autoincr"`
	Name        string `xorm:"UNIQUE NOT NULL"`
	Slug        string `xorm:"UNIQUE NOT NULL"`
	Description string `xorm:"TEXT"`
	Mode        string `xorm:"NOT NULL"`
	Cursor      int    `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentModelGroupV360) TableName() string {
	return "agent_model_group"
}

// agentModelGroupMemberV360 is the frozen shape of agent_model_group_member, one target
// of a group in priority order.
type agentModelGroupMemberV360 struct {
	ID       int64 `xorm:"pk autoincr"`
	GroupID  int64 `xorm:"INDEX NOT NULL"`
	Position int   `xorm:"NOT NULL"`

	ProviderID int64  `xorm:"NOT NULL DEFAULT 0"`
	Model      string `xorm:"NOT NULL DEFAULT ''"`

	MemberGroupID int64              `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix   timeutil.TimeStamp `xorm:"created NOT NULL"`
}

func (agentModelGroupMemberV360) TableName() string {
	return "agent_model_group_member"
}

// AddAgentModelGroupTables creates the model group tables: a group is a set of
// routable members, the way a load balancer fronts several backends.
func AddAgentModelGroupTables(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentModelGroupV360), new(agentModelGroupMemberV360))
	return err
}

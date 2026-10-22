// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentSkillV372 is the frozen shape of the table holding the procedures the agent can open.
type agentSkillV372 struct {
	ID          int64  `xorm:"pk autoincr"`
	Name        string `xorm:"UNIQUE NOT NULL"`
	Description string `xorm:"VARCHAR(255) NOT NULL"`
	Body        string `xorm:"TEXT NOT NULL"`
	Scope       string `xorm:"VARCHAR(16) NOT NULL DEFAULT 'common'"`
	Enabled     bool   `xorm:"NOT NULL DEFAULT TRUE"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentSkillV372) TableName() string {
	return "agent_skill"
}

// AddAgentSkill stores the skills an agent may open by name.
func AddAgentSkill(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentSkillV372))
	return err
}

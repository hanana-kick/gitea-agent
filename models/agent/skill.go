// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// Skill scopes say which sessions a skill is for. The reach grows the same way it does for
// tools: a common skill is for every session, a repository one is for sessions that work on
// a repository, and a server one is for sessions that look after the instance.
const (
	SkillScopeCommon     = "common"
	SkillScopeRepository = "repository"
	SkillScopeServer     = "server"
)

// Skill is one procedure the agent can open by name. Only its name and description reach
// the prompt, so a skill costs almost no context until the model asks for its body.
type Skill struct {
	ID   int64  `xorm:"pk autoincr"`
	Name string `xorm:"UNIQUE NOT NULL"` // what the model calls it by
	// Description is the one line the prompt shows: what the skill is for.
	Description string `xorm:"VARCHAR(255) NOT NULL"`
	Body        string `xorm:"TEXT NOT NULL"`
	Scope       string `xorm:"VARCHAR(16) NOT NULL DEFAULT 'common'"` // common, repository, server
	// Enabled gates the skill entirely: a disabled skill reaches neither prompt nor model.
	Enabled bool `xorm:"NOT NULL DEFAULT TRUE"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (Skill) TableName() string {
	return "agent_skill"
}

func init() {
	db.RegisterModel(new(Skill))
}

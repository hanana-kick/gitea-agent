// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentProviderV356 is the frozen shape of agent_provider, the BYOK LLM provider keys.
type agentProviderV356 struct {
	ID         int64  `xorm:"pk autoincr"`
	UserID     int64  `xorm:"INDEX NOT NULL"`
	Name       string `xorm:"NOT NULL"`
	Provider   string `xorm:"NOT NULL"`
	BaseURL    string
	Model      string `xorm:"NOT NULL"`
	ModelsJSON string `xorm:"TEXT"`

	APIKeyID    int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
	HeadersJSON string             `xorm:"TEXT"`
	HasKey      bool               `xorm:"-"`
	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentProviderV356) TableName() string {
	return "agent_provider"
}

// AddAgentProviderTable creates the agent_provider table for BYOK LLM provider keys.
func AddAgentProviderTable(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentProviderV356))
	return err
}

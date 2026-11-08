// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentProviderV359 is the frozen shape of agent_provider with the custom request
// headers column.
type agentProviderV359 struct {
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

func (agentProviderV359) TableName() string {
	return "agent_provider"
}

// AddAgentProviderHeadersColumn adds the custom request headers column to agent_provider.
func AddAgentProviderHeadersColumn(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentProviderV359))
	return err
}

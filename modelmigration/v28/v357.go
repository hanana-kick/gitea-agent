// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	agent_model "gitea.dev/models/agent"
	"xorm.io/xorm"
)

// AddAgentProviderHeadersColumn adds the custom request headers column to agent_provider.
func AddAgentProviderHeadersColumn(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agent_model.Provider))
	return err
}

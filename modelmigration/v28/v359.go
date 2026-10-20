// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"

	"gitea.dev/modelmigration/base"
	agent_model "gitea.dev/models/agent"

	"xorm.io/xorm"
)

// AddAgentConversationTables creates the session and transcript tables of the agent.
func AddAgentConversationTables(_ context.Context, x base.EngineMigration) error {
	_, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agent_model.Conversation), new(agent_model.Message))
	return err
}

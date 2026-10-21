// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
)

// ModeOf returns the permission mode of a tool; unknown tools default to llm-decided.
func ModeOf(ctx context.Context, tool string) (string, bool, error) {
	row := &agent_model.ToolPermission{}
	has, err := db.GetEngine(ctx).Where("tool = ?", tool).Get(row)
	if err != nil {
		return "", false, fmt.Errorf("get tool permission %q: %w", tool, err)
	}
	if !has {
		return PermissionLLM, true, nil
	}
	if !row.Enabled {
		return "", false, nil
	}
	switch row.Mode {
	case PermissionManual, PermissionAllowAll:
		return row.Mode, true, nil
	}
	return PermissionLLM, true, nil
}

// SetMode stores the permission mode of a tool.
func SetMode(ctx context.Context, tool, mode string, enabled bool) error {
	row := &agent_model.ToolPermission{}
	has, err := db.GetEngine(ctx).Where("tool = ?", tool).Get(row)
	if err != nil {
		return fmt.Errorf("get tool permission %q: %w", tool, err)
	}
	if !has {
		return db.Insert(ctx, &agent_model.ToolPermission{Tool: tool, Mode: mode, Enabled: enabled})
	}
	_, err = db.GetEngine(ctx).ID(row.ID).Cols("mode", "enabled").Update(&agent_model.ToolPermission{Mode: mode, Enabled: enabled})
	return err
}

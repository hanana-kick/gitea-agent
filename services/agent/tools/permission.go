// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
)

// Decision is what the permission check made of one call.
type Decision string

const (
	// DecisionRun lets the tool run.
	DecisionRun Decision = "run"
	// DecisionAsk means the user has to allow the call first.
	DecisionAsk Decision = "ask"
	// DecisionDenied means the call may not happen.
	DecisionDenied Decision = "denied"
)

// Available returns the tools this session may use, sorted by name: the registry narrowed
// by scope, by what the admin left enabled, and by whether the tool has anything to offer
// this session.
func Available(ctx context.Context, session Session) ([]Tool, error) {
	list := Registry()
	usable := make([]Tool, 0, len(list))
	for _, tool := range list {
		if !tool.Scope().AllowedFor(session.Kind) {
			continue
		}
		offered, err := offeredTo(ctx, tool, session)
		if err != nil {
			return nil, err
		}
		if !offered {
			continue
		}
		_, enabled, err := ModeOf(ctx, tool.Name())
		if err != nil {
			return nil, err
		}
		if enabled {
			usable = append(usable, tool)
		}
	}
	return usable, nil
}

// offeredTo reports whether a tool has anything to offer this session; a tool without a
// condition is always offered.
func offeredTo(ctx context.Context, tool Tool, session Session) (bool, error) {
	conditional, ok := tool.(Conditional)
	if !ok {
		return true, nil
	}
	return conditional.Offered(ctx, session)
}

// Check reports how one call may proceed: run, ask its user, or not at all.
func Check(ctx context.Context, session Session, name string) (Tool, Decision, error) {
	if strings.TrimSpace(name) == "" {
		return nil, DecisionDenied, errors.New("that call named no tool")
	}
	tool, ok := Find(name)
	if !ok {
		return nil, DecisionDenied, fmt.Errorf("unknown tool %q", name)
	}
	if !tool.Scope().AllowedFor(session.Kind) {
		return tool, DecisionDenied, nil
	}
	// a tool that is not offered here is not callable here either, whatever the model asks —
	// and what a tool is offered needs the whole session, not just its kind
	offered, err := offeredTo(ctx, tool, session)
	if err != nil {
		return tool, DecisionDenied, err
	}
	if !offered {
		return tool, DecisionDenied, nil
	}
	mode, enabled, err := ModeOf(ctx, name)
	if err != nil {
		return tool, DecisionDenied, err
	}
	switch {
	case !enabled:
		return tool, DecisionDenied, nil
	case mode == PermissionManual:
		return tool, DecisionAsk, nil
	default:
		return tool, DecisionRun, nil
	}
}

// ModeOf returns the permission mode of a tool and whether it is offered at all. A tool that
// reads what the user may read is offered at once and runs when the model decides; a tool that
// writes waits for an admin to switch it on and asks its reader before it runs. The mode of a
// disabled tool is still reported, so the admin page can show what it will go back to.
func ModeOf(ctx context.Context, tool string) (string, bool, error) {
	defaultMode, defaultEnabled := PermissionLLM, true
	if registered, ok := Find(tool); ok && Writes(registered) {
		defaultMode, defaultEnabled = PermissionManual, false
	}
	row := &agent_model.ToolPermission{}
	has, err := db.GetEngine(ctx).Where("tool = ?", tool).Get(row)
	if err != nil {
		return "", false, fmt.Errorf("get tool permission %q: %w", tool, err)
	}
	if !has {
		return defaultMode, defaultEnabled, nil
	}
	switch row.Mode {
	case PermissionManual, PermissionAllowAll:
		return row.Mode, row.Enabled, nil
	}
	return PermissionLLM, row.Enabled, nil
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

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// GetModelRoleRefs returns the model reference of every role, roles without a model included.
func GetModelRoleRefs(ctx context.Context) (map[string]string, error) {
	roles := make([]*agent_model.ModelRole, 0, len(agent_model.ModelRoleOptions))
	if err := db.GetEngine(ctx).Find(&roles); err != nil {
		return nil, fmt.Errorf("find model roles: %w", err)
	}
	refs := make(map[string]string, len(agent_model.ModelRoleOptions))
	for _, role := range agent_model.ModelRoleOptions {
		refs[role] = ""
	}
	for _, role := range roles {
		refs[role.Role] = role.ModelRef
	}
	return refs, nil
}

// ModelRoleRef returns the model a role answers with; it is empty when none is assigned.
func ModelRoleRef(ctx context.Context, role string) (string, error) {
	if !agent_model.IsValidModelRole(role) {
		return "", fmt.Errorf("unknown model role %q", role)
	}
	row := &agent_model.ModelRole{}
	has, err := db.GetEngine(ctx).Where("role = ?", role).Get(row)
	if err != nil {
		return "", fmt.Errorf("get model role %q: %w", role, err)
	}
	if !has {
		return "", nil
	}
	return row.ModelRef, nil
}

// SetModelRoleRef assigns a model to a role; an empty reference clears it.
func SetModelRoleRef(ctx context.Context, role, ref string) error {
	if !agent_model.IsValidModelRole(role) {
		return fmt.Errorf("unknown model role %q", role)
	}
	row := &agent_model.ModelRole{}
	has, err := db.GetEngine(ctx).Where("role = ?", role).Get(row)
	if err != nil {
		return fmt.Errorf("get model role %q: %w", role, err)
	}
	if !has {
		return db.Insert(ctx, &agent_model.ModelRole{Role: role, ModelRef: ref})
	}
	_, err = db.GetEngine(ctx).ID(row.ID).Cols("model_ref", "updated_unix").
		Update(&agent_model.ModelRole{ModelRef: ref, UpdatedUnix: timeutil.TimeStampNow()})
	return err
}

// ModelRoleForKind returns the role a session of the given kind answers with.
func ModelRoleForKind(kind string) string {
	if agent_model.IsValidModelRole(kind) {
		return kind
	}
	return agent_model.ModelRoleServer
}

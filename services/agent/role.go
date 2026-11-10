// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

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

// ForgetModelRefs clears the roles whose model reference matches a LIKE pattern. A deleted model
// has to take its assignments with it: a role left naming one fails at the next run, and nothing
// on the settings page shows that it will.
func ForgetModelRefs(ctx context.Context, patterns ...string) error {
	if len(patterns) == 0 {
		return nil
	}
	sess := db.GetEngine(ctx)
	for _, pattern := range patterns {
		if _, err := sess.Where("model_ref LIKE ?", pattern).Cols("model_ref", "updated_unix").
			Update(&agent_model.ModelRole{ModelRef: "", UpdatedUnix: timeutil.TimeStampNow()}); err != nil {
			return fmt.Errorf("clear roles naming %q: %w", pattern, err)
		}
	}
	return nil
}

// modelRefPatternForProvider matches every role naming a model of one provider.
func modelRefPatternForProvider(providerID int64) string {
	return fmt.Sprintf("%s:%d/%%", modelRefProvider, providerID)
}

// RoleModels are the models an instance answers with: the cheap one for the small jobs, the strong
// one for the work, and the decision model that judges what a person would otherwise be asked. The
// decision model is optional: an instance that names none decides with its strong model.
type RoleModels struct {
	Cheap    string
	Strong   string
	Decision string
}

// Preset names of the ways the roles are split between the two tiers.
const (
	// RolePresetEconomy answers everything with the cheap model.
	RolePresetEconomy = "economy"
	// RolePresetBalanced does the work with the strong model and the small jobs with the cheap.
	RolePresetBalanced = "balanced"
	// RolePresetQuality answers everything with the strong model.
	RolePresetQuality = "quality"
)

// RolePresets lists every preset with the tier each role is answered from, so the page can show
// what a preset would do before it is applied.
var RolePresets = []string{RolePresetEconomy, RolePresetBalanced, RolePresetQuality}

// AssignRoles assigns every role a model: the decision roles judge with the decision model, the
// small jobs are answered with the cheap one, and the work with the strong one. The preset says
// how much of that an instance wants to pay for.
func AssignRoles(ctx context.Context, preset string, models RoleModels) error {
	if !slices.Contains(RolePresets, preset) {
		return fmt.Errorf("unknown preset %q", preset)
	}
	if models.Cheap == "" || models.Strong == "" {
		return errors.New("both the cheap and the strong model are needed")
	}
	decision := models.Decision
	if decision == "" {
		decision = models.Strong
	}
	for _, role := range agent_model.ModelRoleOptions {
		ref := models.Strong
		switch {
		case agent_model.ModelRoleClassOf(role) == agent_model.ModelRoleClassDecision:
			ref = decision
		case preset == RolePresetEconomy:
			ref = models.Cheap
		case preset == RolePresetBalanced && agent_model.IsLightModelRole(role):
			ref = models.Cheap
		}
		if err := SetModelRoleRef(ctx, role, ref); err != nil {
			return err
		}
	}
	return nil
}

// ModelRoleForKind returns the role a session of the given kind answers with.
func ModelRoleForKind(kind string) string {
	if agent_model.IsValidModelRole(kind) {
		return kind
	}
	return agent_model.ModelRoleServer
}

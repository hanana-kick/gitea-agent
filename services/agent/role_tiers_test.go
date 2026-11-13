// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/unittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A role says what it needs of a model: one that decides, or one that writes. An instance pays for
// a cheap model for the small jobs, a strong one for the work, and may name a decision model for
// what a person would otherwise be asked.
func TestAssignRolesSplitsTheRolesByClassAndTier(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	models := RoleModels{
		Cheap: "provider:1/cheap-model", Strong: "provider:2/strong-model",
		Decision: "provider:3/decision-model",
	}

	// what each role needs of its model: the decision roles judge, the others write
	assert.Equal(t, agent_model.ModelRoleClassDecision, agent_model.ModelRoleClassOf(agent_model.ModelRoleDecision))
	for _, role := range []string{agent_model.ModelRoleResident, agent_model.ModelRoleServer, agent_model.ModelRoleRepository, agent_model.ModelRoleSummary} {
		assert.Equal(t, agent_model.ModelRoleClassModel, agent_model.ModelRoleClassOf(role), "%s decides?", role)
	}
	assert.Equal(t, []string{agent_model.ModelRoleSummary}, agent_model.ModelRoleLightRoles, "the light roles are the small jobs, not the decisions")
	assert.Equal(t, []string{agent_model.ModelRoleResident, agent_model.ModelRoleServer, agent_model.ModelRoleRepository, agent_model.ModelRoleSummary},
		agent_model.ModelRoleRolesOfClass(agent_model.ModelRoleClassModel))
	assert.Equal(t, []string{agent_model.ModelRoleDecision}, agent_model.ModelRoleRolesOfClass(agent_model.ModelRoleClassDecision))

	require.NoError(t, AssignRoles(ctx, RolePresetEconomy, models))
	for _, role := range agent_model.ModelRoleOptions {
		ref, err := ModelRoleRef(ctx, role)
		require.NoError(t, err)
		want := models.Cheap
		if agent_model.ModelRoleClassOf(role) == agent_model.ModelRoleClassDecision {
			// what decides is decided with the decision model whatever the preset
			want = models.Decision
		}
		assert.Equal(t, want, ref, "the economy preset answers %s with something else", role)
	}

	require.NoError(t, AssignRoles(ctx, RolePresetBalanced, models))
	for _, role := range agent_model.ModelRoleOptions {
		ref, err := ModelRoleRef(ctx, role)
		require.NoError(t, err)
		want := models.Strong
		switch {
		case agent_model.ModelRoleClassOf(role) == agent_model.ModelRoleClassDecision:
			want = models.Decision
		case agent_model.IsLightModelRole(role):
			want = models.Cheap
		}
		assert.Equal(t, want, ref, "the balanced preset answers %s with the wrong model", role)
	}

	require.NoError(t, AssignRoles(ctx, RolePresetQuality, models))
	for _, role := range agent_model.ModelRoleOptions {
		ref, err := ModelRoleRef(ctx, role)
		require.NoError(t, err)
		want := models.Strong
		if agent_model.ModelRoleClassOf(role) == agent_model.ModelRoleClassDecision {
			want = models.Decision
		}
		assert.Equal(t, want, ref, "the quality preset answers %s with something else", role)
	}

	// an instance that names no decision model decides with the strong one
	withoutDecision := RoleModels{Cheap: models.Cheap, Strong: models.Strong}
	require.NoError(t, AssignRoles(ctx, RolePresetBalanced, withoutDecision))
	ref, err := ModelRoleRef(ctx, agent_model.ModelRoleDecision)
	require.NoError(t, err)
	assert.Equal(t, models.Strong, ref, "deciding fell back to something other than the strong model")

	require.Error(t, AssignRoles(ctx, "cheapest", models))
	require.Error(t, AssignRoles(ctx, RolePresetBalanced, RoleModels{Cheap: models.Cheap}))
}

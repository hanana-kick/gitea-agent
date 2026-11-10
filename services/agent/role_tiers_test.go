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

// An instance pays for two models at most: a cheap one that is enough for the small jobs and a
// strong one for the work. A preset is one way of splitting the roles between them.
func TestAssignRoleTiersSplitsTheRolesBetweenTwoModels(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	tiers := RoleTiers{Cheap: "provider:1/cheap-model", Strong: "provider:2/strong-model"}

	require.NoError(t, AssignRoleTiers(ctx, RolePresetEconomy, tiers))
	for _, role := range agent_model.ModelRoleOptions {
		ref, err := ModelRoleRef(ctx, role)
		require.NoError(t, err)
		assert.Equal(t, tiers.Cheap, ref, "the economy preset answers %s with something else", role)
	}

	require.NoError(t, AssignRoleTiers(ctx, RolePresetBalanced, tiers))
	for _, role := range agent_model.ModelRoleOptions {
		ref, err := ModelRoleRef(ctx, role)
		require.NoError(t, err)
		want := tiers.Strong
		if agent_model.IsLightModelRole(role) {
			want = tiers.Cheap
		}
		assert.Equal(t, want, ref, "the balanced preset answers %s with the wrong tier", role)
	}

	require.NoError(t, AssignRoleTiers(ctx, RolePresetQuality, tiers))
	for _, role := range agent_model.ModelRoleOptions {
		ref, err := ModelRoleRef(ctx, role)
		require.NoError(t, err)
		assert.Equal(t, tiers.Strong, ref, "the quality preset answers %s with something else", role)
	}

	// the light roles are the small jobs: naming a session, and reviewing what autopilot does
	assert.True(t, agent_model.IsLightModelRole(agent_model.ModelRoleSummary))
	assert.True(t, agent_model.IsLightModelRole(agent_model.ModelRoleReview))
	assert.False(t, agent_model.IsLightModelRole(agent_model.ModelRoleRepository))

	require.Error(t, AssignRoleTiers(ctx, "cheapest", tiers))
	require.Error(t, AssignRoleTiers(ctx, RolePresetBalanced, RoleTiers{Cheap: tiers.Cheap}))
}

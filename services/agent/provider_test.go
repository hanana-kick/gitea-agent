// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyValueDecryptsStoredSecret(t *testing.T) {
	encrypted, err := secret_module.EncryptSecret(setting.SecretKey, "sk-stored")
	require.NoError(t, err)
	encryptedWrongKey, err := secret_module.EncryptSecret("another-key", "sk-stored")
	require.NoError(t, err)

	assert.Equal(t, "sk-stored", APIKeyValue(context.Background(), &agent_model.APIKey{SecretEnc: encrypted}))
	// a key encrypted with a different secret can not be recovered
	assert.Empty(t, APIKeyValue(context.Background(), &agent_model.APIKey{SecretEnc: encryptedWrongKey}))
	// a provider without a bound key resolves to nothing
	assert.Empty(t, ResolveProviderAPIKey(context.Background(), &agent_model.Provider{}))
}

// The model a role answers with has to be one that exists: a role left naming a deleted provider
// fails at the next run, and nothing on the settings page shows that it will.
func TestDeletingAModelForgetsTheRolesAndMembershipsThatNamedIt(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	provider := &agent_model.Provider{UserID: owner.ID, Name: "stub", Provider: "openai", BaseURL: "http://127.0.0.1:9/v1", Model: "stub-model"}
	require.NoError(t, db.Insert(ctx, provider))
	group := &agent_model.ModelGroup{Name: "stub group", Slug: "stub-group", Mode: agent_model.GroupModeFallback}
	require.NoError(t, db.Insert(ctx, group))
	require.NoError(t, saveGroupMembers(ctx, group.ID, []GroupMemberTarget{{ProviderID: provider.ID, Model: "stub-model"}}))
	outer := &agent_model.ModelGroup{Name: "outer group", Slug: "outer-group", Mode: agent_model.GroupModeFallback}
	require.NoError(t, db.Insert(ctx, outer))
	require.NoError(t, saveGroupMembers(ctx, outer.ID, []GroupMemberTarget{{GroupID: group.ID}}))

	require.NoError(t, SetModelRoleRef(ctx, agent_model.ModelRoleRepository, ModelRefForProvider(provider.ID, "stub-model")))
	require.NoError(t, SetModelRoleRef(ctx, agent_model.ModelRoleSummary, ModelRefForGroup(group.Slug)))

	require.NoError(t, DeleteProvider(ctx, provider.ID))
	assertRoleUnset(ctx, t, agent_model.ModelRoleRepository)
	members, err := GetGroupMembers(ctx, group.ID)
	require.NoError(t, err)
	assert.Empty(t, members, "the group kept a member naming the deleted provider")

	require.NoError(t, DeleteModelGroup(ctx, group.ID))
	assertRoleUnset(ctx, t, agent_model.ModelRoleSummary)
	members, err = GetGroupMembers(ctx, outer.ID)
	require.NoError(t, err)
	assert.Empty(t, members, "the outer group kept a membership naming the deleted group")
}

func assertRoleUnset(ctx context.Context, t *testing.T, role string) {
	t.Helper()
	ref, err := ModelRoleRef(ctx, role)
	require.NoError(t, err)
	assert.Empty(t, ref, "role %q still names a model that is gone", role)
}

func TestAPIKeyRejectsEmptyFields(t *testing.T) {
	_, err := CreateAPIKey(context.Background(), 1, "", "sk")
	assert.ErrorIs(t, err, ErrAPIKeyInvalid{})

	_, err = CreateAPIKey(context.Background(), 1, "name", "   ")
	assert.ErrorIs(t, err, ErrAPIKeyInvalid{})

	assert.ErrorIs(t, ReplaceAPIKeyValue(context.Background(), 1, ""), ErrAPIKeyInvalid{})
}

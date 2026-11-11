// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"strings"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeGroupName(t *testing.T) {
	assert.Equal(t, "Code Review", agent_model.NormalizeGroupName("  Code   Review "))
	assert.Equal(t, "코드 리뷰", agent_model.NormalizeGroupName("코드   리뷰"))
	assert.Empty(t, agent_model.NormalizeGroupName("   "))
}

func TestGroupSlugFromName(t *testing.T) {
	assert.Equal(t, "code-review", agent_model.GroupSlugFromName("Code Review"))
	assert.Equal(t, "code-review-v2", agent_model.GroupSlugFromName("Code Review v2"))
	assert.Equal(t, "a-b-c", agent_model.GroupSlugFromName("  A / B : C  "))
	assert.Equal(t, "gpt-4o-mini", agent_model.GroupSlugFromName("gpt-4o-mini"))

	// names without a readable latin word yield no identifier: the service generates one
	assert.Empty(t, agent_model.GroupSlugFromName("코드 리뷰 (고성능)"))
	assert.Empty(t, agent_model.GroupSlugFromName("무료 모음 2"))
	assert.Empty(t, agent_model.GroupSlugFromName("2024"))
	assert.Empty(t, agent_model.GroupSlugFromName("   "))

	// the identifier stays within the column size
	long := agent_model.GroupSlugFromName(strings.Repeat("word ", 40))
	assert.LessOrEqual(t, len(long), 64)
	assert.False(t, strings.HasPrefix(long, "-"))
	assert.False(t, strings.HasSuffix(long, "-"))
}

// groupClassFixture stores a provider that offers one model of each kind, so a group can be built
// out of either.
func groupClassFixture(t *testing.T) (*agent_model.Provider, GroupMemberTarget, GroupMemberTarget) {
	t.Helper()
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	key, err := CreateAPIKey(ctx, owner.ID, "class-key", "sk-class")
	require.NoError(t, err)
	provider := &agent_model.Provider{UserID: owner.ID, Name: "class gateway", Provider: "openrouter", BaseURL: "https://example.invalid", Model: "acme/chat-one", APIKeyID: key.ID}
	require.NoError(t, db.Insert(ctx, provider))
	require.NoError(t, provider.SetModels([]agent_model.ProviderModel{
		{ID: "acme/chat-one", Chat: true},
		{ID: "only/serves-decisions", Decisions: true},
	}))
	_, err = db.GetEngine(ctx).ID(provider.ID).Cols("models_json").Update(provider)
	require.NoError(t, err)

	t.Cleanup(func() {
		ctx := context.Background()
		groups, _, err := FindModelGroups(ctx, agent_model.FindModelGroupsOptions{ListOptions: db.ListOptionsAll})
		if err == nil {
			for _, group := range groups {
				_ = DeleteModelGroup(ctx, group.ID)
			}
		}
		_, _ = db.DeleteByID[agent_model.Provider](ctx, provider.ID)
		_, _ = db.DeleteByID[agent_model.APIKey](ctx, key.ID)
	})

	return provider,
		GroupMemberTarget{ProviderID: provider.ID, Model: "acme/chat-one"},
		GroupMemberTarget{ProviderID: provider.ID, Model: "only/serves-decisions"}
}

// A group fronts one kind of model, so it may only hold what answers the same, whether that is a
// model of a provider or another group.
func TestGroupHoldsOnlyWhatItFronts(t *testing.T) {
	ctx := context.Background()
	_, chatMember, decisionMember := groupClassFixture(t)

	_, err := CreateModelGroup(ctx, ModelGroupOptions{Name: "deciders", Decisions: true, Members: []GroupMemberTarget{chatMember}})
	assert.ErrorIs(t, err, ErrModelGroupInvalid{}, "a group that answers decisions took a model that writes")

	chat, err := CreateModelGroup(ctx, ModelGroupOptions{Name: "writers", Members: []GroupMemberTarget{chatMember}})
	require.NoError(t, err)
	deciders, err := CreateModelGroup(ctx, ModelGroupOptions{Name: "deciders", Decisions: true, Members: []GroupMemberTarget{decisionMember}})
	require.NoError(t, err)

	_, err = CreateModelGroup(ctx, ModelGroupOptions{Name: "mixed", Decisions: true, Members: []GroupMemberTarget{{GroupID: chat.ID}}})
	assert.ErrorIs(t, err, ErrModelGroupInvalid{}, "a group that answers decisions took a group that writes")

	_, err = CreateModelGroup(ctx, ModelGroupOptions{Name: "mixed", Members: []GroupMemberTarget{{GroupID: deciders.ID}}})
	assert.ErrorIs(t, err, ErrModelGroupInvalid{}, "a group that writes took a group that answers decisions")
}

// A group that fronts one kind is what the roles of that kind answer with: a role refuses a group
// that fronts the other kind, and a group a role answers with can not change what it fronts while
// the role, or a group holding it, still expects the old kind.
func TestGroupClassFollowsTheRolesAndHolders(t *testing.T) {
	ctx := context.Background()
	_, chatMember, decisionMember := groupClassFixture(t)

	chat, err := CreateModelGroup(ctx, ModelGroupOptions{Name: "writers", Members: []GroupMemberTarget{chatMember}})
	require.NoError(t, err)
	deciders, err := CreateModelGroup(ctx, ModelGroupOptions{Name: "deciders", Decisions: true, Members: []GroupMemberTarget{decisionMember}})
	require.NoError(t, err)

	assert.Error(t, SetModelRoleRef(ctx, agent_model.ModelRoleDecision, ModelRefForGroup(chat.Slug)),
		"a group that writes was assigned to the role that decides")
	require.NoError(t, SetModelRoleRef(ctx, agent_model.ModelRoleDecision, ModelRefForGroup(deciders.Slug)))

	_, err = UpdateModelGroup(ctx, deciders.ID, ModelGroupOptions{Name: "deciders", Members: []GroupMemberTarget{chatMember}})
	assert.ErrorIs(t, err, ErrModelGroupInvalid{}, "a group the decision role answers with changed to what writes")

	holder, err := CreateModelGroup(ctx, ModelGroupOptions{Name: "holder", Members: []GroupMemberTarget{{GroupID: chat.ID}}})
	require.NoError(t, err)
	_, err = UpdateModelGroup(ctx, chat.ID, ModelGroupOptions{Name: "writers", Decisions: true, Members: []GroupMemberTarget{decisionMember}})
	assert.ErrorIs(t, err, ErrModelGroupInvalid{}, "a group that writes changed while another group held it")

	// and what a group fronts reaches the pickers the roles of that kind are chosen from
	split, err := ModelOptionsByClass(ctx, 500)
	require.NoError(t, err)
	ref := ModelRefForGroup(deciders.Slug)
	assert.True(t, containsRef(split[agent_model.ModelRoleClassDecision], ref), "a group that answers decisions is not offered to the role that decides")
	assert.False(t, containsRef(split[agent_model.ModelRoleClassModel], ref), "a group that answers decisions is offered to a role that writes")
	assert.True(t, containsRef(split[agent_model.ModelRoleClassModel], ModelRefForGroup(holder.Slug)))
}

func containsRef(options []ModelOption, ref string) bool {
	for _, option := range options {
		if option.Ref == ref {
			return true
		}
	}
	return false
}

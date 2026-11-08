// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The skill tool answers with the body of a skill the session may open, and with the same
// refusal for every name it may not: one nothing answers to, one of a wider scope, and one
// the admin switched off.
func TestSkillToolOpensOnlyTheSkillsInReach(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	store := func(name, scope string, enabled bool, body string) {
		require.NoError(t, db.Insert(ctx, &agent_model.Skill{
			Name: name, Description: "a skill of the edge cases", Body: body,
			Scope: scope, Enabled: enabled,
		}))
	}
	body := "1. Read the issue\n2. Answer it"
	store("edge-in-reach", agent_model.SkillScopeCommon, true, body)
	store("edge-elsewhere", agent_model.SkillScopeServer, true, body)
	store("edge-disabled", agent_model.SkillScopeCommon, false, body)
	long := strings.Repeat("a step of the skill that is long enough to be worth clipping\n", 200)
	store("edge-long", agent_model.SkillScopeCommon, true, long)

	open := func(session Session, name string) string {
		out, err := readSkillTool{}.Run(ctx, nil, session, `{"name":"`+name+`"}`)
		require.NoError(t, err)
		return out
	}
	repository := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	assert.Equal(t, "Skill edge-in-reach:\n\n"+body, open(repository, "edge-in-reach"))

	// a body the prompt cannot afford comes back cut, so one skill never fills the run that
	// opens it
	clipped := open(repository, "edge-long")
	assert.Contains(t, clipped, "Skill edge-long:")
	assert.True(t, strings.HasSuffix(clipped, "[truncated]"), "the body was longer than the prompt limit")
	assert.Less(t, len(clipped), len(long))

	// the same skill the repository session was refused opens where its scope reaches
	server := Session{Kind: agent_model.ConversationKindServer}
	assert.Equal(t, "Skill edge-elsewhere:\n\n"+body, open(server, "edge-elsewhere"))

	refusal := "No skill by that name is available in this session. Use a name from the skills listed for this session."
	for _, name := range []string{"edge-nothing-here", "edge-elsewhere", "edge-disabled"} {
		assert.Equal(t, refusal, open(repository, name), name)
	}
}

// A session the agent answers for writes as the agent: the user who asked is not the account
// the release is published under.
func TestActorOfWritesAsTheAgentOfItsSession(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	agent := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	asker := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1, ActorID: agent.ID}

	out, err := createReleaseTool{}.Run(ctx, asker, session, `{"tag":"v9.9.7","title":"scripted","notes":"the first scripted release"}`)
	require.NoError(t, err)
	require.Contains(t, out, "Released v9.9.7")

	release, err := repo_model.GetRelease(ctx, 1, "v9.9.7")
	require.NoError(t, err)
	assert.Equal(t, agent.ID, release.PublisherID)
	assert.NotEqual(t, asker.ID, release.PublisherID)

	// without an actor the write stays with the user who asked
	session.ActorID = 0
	_, err = createReleaseTool{}.Run(ctx, agent, session, `{"tag":"v9.9.8","title":"scripted again","notes":"the second scripted release"}`)
	require.NoError(t, err)
	release, err = repo_model.GetRelease(ctx, 1, "v9.9.8")
	require.NoError(t, err)
	assert.Equal(t, agent.ID, release.PublisherID)
}

// resolveRepo reads the name a session is given: the one of its own repository whatever case
// it is written in, a name that is not owner/name not at all, and one its user cannot read
// as a refusal rather than a failure.
func TestResolveRepoChecksTheNameItIsGiven(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	repository, err := resolveRepo(ctx, owner, Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}, "USER2/RePo1", unit_model.TypeCode)
	require.NoError(t, err)
	assert.Equal(t, int64(1), repository.ID)

	unbound := Session{Kind: agent_model.ConversationKindServer}
	_, err = resolveRepo(ctx, owner, unbound, "repo1", unit_model.TypeCode)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner/name")

	stranger := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	_, err = resolveRepo(ctx, stranger, unbound, "user2/repo2", unit_model.TypeCode)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "You cannot read the code of user2/repo2.")
}

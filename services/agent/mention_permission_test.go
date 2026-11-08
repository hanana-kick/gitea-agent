// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mentionIn is the smallest mention there is: the agent is named in a thread, with the agent
// account it works as in place.
func mentionIn(t *testing.T, agentUser, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue) {
	t.Helper()
	defer func(previous string) { setting.Agent.Username = previous }(setting.Agent.Username)
	setting.Agent.Username = agentUser.Name
	requestMention(context.Background(), doer, repo, issue, "@"+agentUser.Name+" have a look",
		[]*user_model.User{agentUser})
}

func sessionsOf(t *testing.T, repoID int64) []*agent_model.Conversation {
	t.Helper()
	sessions := make([]*agent_model.Conversation, 0, 4)
	require.NoError(t, db.GetEngine(context.Background()).
		Where("kind = ?", agent_model.ConversationKindRepository).
		And("repo_id = ?", repoID).Find(&sessions))
	return sessions
}

func runsOf(t *testing.T, repoID int64) int {
	t.Helper()
	runs := 0
	for _, session := range sessionsOf(t, repoID) {
		count, err := db.GetEngine(context.Background()).Where("conversation_id = ?", session.ID).Count(new(agent_model.Run))
		require.NoError(t, err)
		runs += int(count)
	}
	return runs
}

// A mention from a reader who may not read the repository starts nothing at all: no session,
// no run, and nothing said in the thread.
func TestMentionFromAReaderWithoutAccessIsIgnored(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	// the agent owns the fixture's private repository; this reader has no access to it
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2})
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 4})
	stranger := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	require.True(t, repo.IsPrivate)
	require.False(t, canReadTheThread(ctx, repo, stranger), "fixture changed: user4 can read repo2")

	mentionIn(t, agentUser, stranger, repo, issue)

	assert.Empty(t, sessionsOf(t, repo.ID), "a session was created for a mention from a stranger")
	assert.Zero(t, runsOf(t, repo.ID), "a mention from a reader without access started a run")
}

// A mention in a repository the agent itself may not read is ignored too: the agent works as
// its own account, and there is no work it could do there.
func TestMentionWhereTheAgentHasNoAccessIsIgnored(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	// a private repository of another account, with an issue the agent is named in
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 26})
	require.True(t, repo.IsPrivate)
	require.NotEqual(t, agentUser.ID, repo.OwnerID)
	require.False(t, canReadTheThread(ctx, repo, agentUser), "fixture changed: the agent can read repo26")
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
	issue := &issues_model.Issue{RepoID: repo.ID, Index: 99, PosterID: owner.ID, Title: "agent, look here", Content: "look"}
	require.NoError(t, db.Insert(ctx, issue))

	mentionIn(t, agentUser, owner, repo, issue)

	assert.Empty(t, sessionsOf(t, repo.ID), "a session was created where the agent may not work")
	assert.Zero(t, runsOf(t, repo.ID), "the agent started a run in a repository it may not read")
}

// The tools of a run work as the agent's account, not as the account that asked: a read the
// asker could not make themselves is made by the account the agent works as.
func TestToolsRunWithTheAgentsRights(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	// the agent owns this private repository; the account that asks has no access to it
	privateRepo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 2})
	require.True(t, privateRepo.IsPrivate)

	model := newScriptedModel(t, "gitea_read_issue", `{"index":1}`, "the issue is read")
	conversation, _ := startScriptedAgent(t, model, agent_model.ConversationKindRepository, privateRepo.ID)
	defer func(previous string) { setting.Agent.Username = previous }(setting.Agent.Username)
	setting.Agent.Username = agentUser.Name
	asker := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	require.False(t, canReadTheThread(context.Background(), privateRepo, asker), "fixture changed: user4 can read repo2")

	run := runTurn(t, conversation, asker, "read issue 1")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	require.Equal(t, asker.ID, run.UserID, "the run answers for the user who asked")

	record := toolRecord(t, conversation.ID, "gitea_read_issue")
	require.NotEmpty(t, record)
	assert.Contains(t, record, "user2/repo2")
	assert.NotContains(t, record, "cannot read")
}

// An instance that named no agent account has no rights for a run to work with, so the run
// fails with that reason rather than answering without any tools.
func TestARunWithoutAnAgentAccountFails(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	model := newScriptedModel(t, "", "", "an answer without tools")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	defer func(previous string) { setting.Agent.Username = previous }(setting.Agent.Username)
	setting.Agent.Username = ""

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusFailed, run.Status)
	assert.Contains(t, run.Error, "no agent account")
	assert.Empty(t, model.requestsMade(), "the model was asked without an account to work as")
	_ = agentUser
}

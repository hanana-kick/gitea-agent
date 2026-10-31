// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	notify_service "gitea.dev/services/notify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A comment that names the agent starts a run in the session of that repository, and the run
// reads with the rights of the user who wrote it. A comment the agent wrote, or one that names
// nobody, starts nothing.
func TestMentionStartsARunForTheMentioningUser(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	defer func(previous string) { setting.Agent.Username = previous }(setting.Agent.Username)
	setting.Agent.Username = agentUser.Name
	InitMentions()
	defer func() { notify_service.CommentListener = nil }()

	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 1})
	mentioner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
	comment := &issues_model.Comment{ID: 1, IssueID: issue.ID, Content: "@" + agentUser.Name + " 이 이슈 봐줘"}

	notify_service.CreateIssueComment(ctx, mentioner, repo, issue, comment, []*user_model.User{agentUser})

	sessions, err := findConversations(ctx, agent_model.ConversationKindRepository, agentUser.ID)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, repo.ID, sessions[0].RepoID)

	messages, err := ListMessages(ctx, sessions[0].ID, 20)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Contains(t, messages[len(messages)-1].Content, "[mention]")
	assert.Contains(t, messages[len(messages)-1].Content, comment.Content)

	run, err := LatestRun(ctx, sessions[0].ID)
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, mentioner.ID, run.UserID, "the run reads what the mentioning user may read")

	// the agent answering itself would name it again, forever
	before := len(messages)
	notify_service.CreateIssueComment(ctx, agentUser, repo, issue, comment, []*user_model.User{agentUser})
	after, err := ListMessages(ctx, sessions[0].ID, 20)
	require.NoError(t, err)
	assert.Len(t, after, before)

	// a comment naming nobody starts nothing
	notify_service.CreateIssueComment(ctx, mentioner, repo, issue, comment, nil)
	again, err := ListMessages(ctx, sessions[0].ID, 20)
	require.NoError(t, err)
	assert.Len(t, again, before)
}

// A session the agent owns writes as the agent, so the thread shows who answered while the
// reads stay with the user who asked.
func TestAgentSessionWritesAsTheAgent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	defer func(previous string) { setting.Agent.Username = previous }(setting.Agent.Username)
	setting.Agent.Username = agentUser.Name

	conversation, err := CreateConversation(ctx, agent_model.ConversationKindRepository, agentUser.ID, 1, "agent session")
	require.NoError(t, err)
	session := engine.sessionFor(ctx, conversation)
	assert.Equal(t, agentUser.ID, session.ActorID)

	// a session a user opened writes as that user
	userSession, err := CreateConversation(ctx, agent_model.ConversationKindRepository, 1, 1, "user session")
	require.NoError(t, err)
	assert.Zero(t, engine.sessionFor(ctx, userSession).ActorID)
}

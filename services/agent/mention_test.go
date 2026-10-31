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

// A run whose answer never reached the thread is reported there, and one that did is not said
// again: the surface hears how the work ended, once.
func TestMentionReportsWhatTheThreadDidNotHear(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 1})

	conversation, err := CreateConversation(ctx, agent_model.ConversationKindRepository, 2, repo.ID, "agent session")
	require.NoError(t, err)

	// a run that already succeeded, and a request whose answer the model never posted
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: 1, Status: agent_model.RunStatusSucceeded}
	require.NoError(t, db.Insert(ctx, run))
	request, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleUser, "[mention] …", "")
	require.NoError(t, err)

	agent := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	defer func(previous string) { setting.Agent.Username = previous }(setting.Agent.Username)
	setting.Agent.Username = agent.Name

	// the answer is in the thread already: nothing more is said
	before, err := issues_model.CountComments(ctx, &issues_model.FindCommentsOptions{IssueID: issue.ID})
	require.NoError(t, err)
	reportMention(ctx, request.ID, run.ID, repo, issue)
	after, err := issues_model.CountComments(ctx, &issues_model.FindCommentsOptions{IssueID: issue.ID})
	require.NoError(t, err)
	assert.Equal(t, before, after, "a run that answered in the thread says nothing more")

	// a failed run is said out loud
	failed := &agent_model.Run{ConversationID: conversation.ID, UserID: 1, Status: agent_model.RunStatusFailed, Error: "the model did not answer in time"}
	require.NoError(t, db.Insert(ctx, failed))
	reportMention(ctx, request.ID, failed.ID, repo, issue)
	comments, err := issues_model.FindComments(ctx, &issues_model.FindCommentsOptions{IssueID: issue.ID})
	require.NoError(t, err)
	require.Greater(t, int64(len(comments)), before)
	assert.Contains(t, comments[len(comments)-1].Content, "I could not finish this one")
	assert.Equal(t, agent.ID, comments[len(comments)-1].PosterID)
}

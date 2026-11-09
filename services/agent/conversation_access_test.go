// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The instance-wide sessions are the operator's; a normal user can neither open nor delete them.
func TestConversationResidentAndServerSessionsAreForAdminsOnly(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	admin := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	for _, kind := range []string{agent_model.ConversationKindResident, agent_model.ConversationKindServer} {
		t.Run(kind, func(t *testing.T) {
			session := &agent_model.Conversation{Kind: kind, OwnerID: user.ID}

			allowed, err := CanUseConversation(ctx, admin, session)
			require.NoError(t, err)
			assert.True(t, allowed)

			allowed, err = CanUseConversation(ctx, user, session)
			require.NoError(t, err)
			assert.False(t, allowed, "a non-admin was allowed to open a %s session", kind)
		})
	}
}

// A repository session drives an agent that edits code, so read access to the repository is not
// enough: the owner can use it, a collaborator who may only read can not.
func TestConversationRepositorySessionsNeedWriteAccess(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	reader := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 20})
	writable := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	readOnly := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 24})

	allowed, err := CanUseRepository(ctx, owner, writable)
	require.NoError(t, err)
	assert.True(t, allowed)

	allowed, err = CanUseRepository(ctx, reader, readOnly)
	require.NoError(t, err)
	assert.False(t, allowed, "a reader was allowed to drive an agent that edits code")

	session := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, RepoID: writable.ID}
	allowed, err = CanUseConversation(ctx, owner, session)
	require.NoError(t, err)
	assert.True(t, allowed)

	session = &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, RepoID: readOnly.ID}
	allowed, err = CanUseConversation(ctx, reader, session)
	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestConversationUnknownKindIsNeverUsable(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	admin := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})

	allowed, err := CanUseConversation(ctx, admin, &agent_model.Conversation{Kind: "future"})
	require.NoError(t, err)
	assert.False(t, allowed)
}

// Someone else's session is refused rather than revealed; a missing one is refused as missing,
// so a caller can tell the two apart only by the error it gets.
func TestConversationOfAnotherPersonIsRefused(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	stranger := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})

	session, err := CreateConversation(ctx, agent_model.ConversationKindRepository, owner.ID, repo.ID, "")
	require.NoError(t, err)

	opened, err := GetConversation(ctx, owner, session.ID)
	require.NoError(t, err)
	assert.Equal(t, session.ID, opened.ID)

	_, err = GetConversation(ctx, stranger, session.ID)
	assert.ErrorIs(t, err, ErrConversationDenied{})

	_, err = GetConversation(ctx, owner, 99999)
	assert.ErrorIs(t, err, ErrConversationNotFound{})
}

func TestConversationResidentSessionCanNotBeDeleted(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	admin := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	resident, err := EnsureResidentConversation(ctx)
	require.NoError(t, err)
	assert.ErrorIs(t, DeleteConversation(ctx, resident), ErrResidentUndeletable{})

	_, err = GetConversation(ctx, admin, resident.ID)
	require.NoError(t, err, "the resident session was deleted anyway")

	session, err := CreateConversation(ctx, agent_model.ConversationKindServer, owner.ID, 0, "")
	require.NoError(t, err)
	require.NoError(t, DeleteConversation(ctx, session))
	_, err = GetConversation(ctx, admin, session.ID)
	assert.ErrorIs(t, err, ErrConversationNotFound{})
}

// A session owns more than its transcript: deleting it takes the runs that answered in it, the
// calls a reader answered and the files sent with it, so nothing is left behind.
func TestDeletingASessionTakesTheRunsCallsAndFilesItHolds(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	session, err := CreateConversation(ctx, agent_model.ConversationKindServer, owner.ID, 0, "")
	require.NoError(t, err)
	neighbour, err := CreateConversation(ctx, agent_model.ConversationKindServer, owner.ID, 0, "")
	require.NoError(t, err)

	message := &agent_model.Message{ConversationID: session.ID, Role: agent_model.MessageRoleUser, Content: "read the readme"}
	require.NoError(t, db.Insert(ctx, message))
	run := &agent_model.Run{ConversationID: session.ID, UserID: owner.ID, Status: agent_model.RunStatusWaiting}
	require.NoError(t, db.Insert(ctx, run))
	require.NoError(t, db.Insert(ctx, &agent_model.ToolApproval{
		ConversationID: session.ID, RunID: run.ID, Tool: "gitea_read_file", Arguments: `{"path":"README.md"}`, Status: agent_model.ToolApprovalPending,
	}))
	require.NoError(t, db.Insert(ctx, &agent_model.Attachment{ConversationID: session.ID, MessageID: message.ID, Name: "notes.md", Size: 3}))
	require.NoError(t, db.Insert(ctx, &agent_model.Message{ConversationID: neighbour.ID, Role: agent_model.MessageRoleUser, Content: "stay"}))
	require.NoError(t, db.Insert(ctx, &agent_model.Run{ConversationID: neighbour.ID, UserID: owner.ID, Status: agent_model.RunStatusSucceeded}))

	files := agent_model.ConversationAttachmentDir(session.ID)
	require.NoError(t, os.MkdirAll(files, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(files, "1"), []byte("abc"), 0o600))

	require.NoError(t, DeleteConversation(ctx, session))

	for _, table := range []any{&agent_model.Message{}, &agent_model.Run{}, &agent_model.ToolApproval{}, &agent_model.Attachment{}} {
		count, err := db.GetEngine(ctx).Where("conversation_id = ?", session.ID).Count(table)
		require.NoError(t, err)
		assert.Zero(t, count, "a %T of the deleted session stayed", table)
	}
	_, err = os.Stat(files)
	assert.True(t, os.IsNotExist(err), "the files sent with the session stayed on disk")

	// what belongs to another session is its own, and is not taken with it
	count, err := db.GetEngine(ctx).Where("conversation_id = ?", neighbour.ID).Count(&agent_model.Message{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
	count, err = db.GetEngine(ctx).Where("conversation_id = ?", neighbour.ID).Count(&agent_model.Run{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
}

// The session the agent keeps for a repository is the repository's: a mention in its threads is
// answered in it, so every reader who may write the repository has to see it. A reader who may
// not write it does not, and a session one person started stays their own.
func TestConversationsOfARepositoryShowTheOneTheAgentOwns(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	writer := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	stranger := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	agent := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 5})
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	defer func(previous string) { setting.Agent.Username = previous }(setting.Agent.Username)
	setting.Agent.Username = agent.Name

	// the agent's session for the repository, and one the writer started of their own
	shared, err := CreateConversation(ctx, agent_model.ConversationKindRepository, agent.ID, repo.ID, repo.FullName())
	require.NoError(t, err)
	private, err := CreateConversation(ctx, agent_model.ConversationKindRepository, writer.ID, repo.ID, "mine")
	require.NoError(t, err)
	// and a server session of the agent, which is the operator's and not a repository's
	server, err := CreateConversation(ctx, agent_model.ConversationKindServer, agent.ID, 0, "server")
	require.NoError(t, err)

	listed, err := ListConversations(ctx, writer)
	require.NoError(t, err)
	ids := make([]int64, 0, len(listed.Repositories))
	for _, entry := range listed.Repositories {
		ids = append(ids, entry.Session.ID)
	}
	assert.Contains(t, ids, shared.ID, "the repository's own session is not shown to a reader who may write it")
	assert.Contains(t, ids, private.ID, "a reader's own session of the repository is not shown to them")

	other, err := ListConversations(ctx, stranger)
	require.NoError(t, err)
	otherIDs := make([]int64, 0, len(other.Repositories))
	for _, entry := range other.Repositories {
		otherIDs = append(otherIDs, entry.Session.ID)
	}
	assert.NotContains(t, otherIDs, shared.ID, "a reader who may not write the repository was shown its session")
	assert.NotContains(t, otherIDs, private.ID)
	assert.NotContains(t, otherIDs, server.ID, "a server session is the operator's")

	// the repository keeps one session that mentions are answered in, whoever names the agent
	again, err := repositorySessionOf(ctx, agent, repo)
	require.NoError(t, err)
	assert.Equal(t, shared.ID, again.ID, "a second mention started another session for the repository")
}

// A title is required and capped at 100 runes, counted as runes so a multi-byte one is not cut
// mid-character.
func TestConversationTitleIsRequiredAndClippedToItsLimit(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	session, err := CreateConversation(ctx, agent_model.ConversationKindServer, owner.ID, 0, "Named")
	require.NoError(t, err)

	assert.ErrorIs(t, SetConversationTitle(ctx, session, "   \n "), ErrConversationTitleInvalid{})

	require.NoError(t, SetConversationTitle(ctx, session, strings.Repeat("é", 150)))
	assert.Equal(t, strings.Repeat("é", 100), session.Title)

	stored, exist, err := db.GetByID[agent_model.Conversation](ctx, session.ID)
	require.NoError(t, err)
	require.True(t, exist)
	assert.Equal(t, strings.Repeat("é", 100), stored.Title)
	assert.True(t, stored.TitleSet)
}

func TestConversationSummaryIsClippedToItsLimit(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	session, err := CreateConversation(ctx, agent_model.ConversationKindServer, owner.ID, 0, "Named")
	require.NoError(t, err)

	require.NoError(t, SetConversationSummary(ctx, session, strings.Repeat("é", 250)))
	assert.Equal(t, strings.Repeat("é", 200), session.Summary)

	stored, exist, err := db.GetByID[agent_model.Conversation](ctx, session.ID)
	require.NoError(t, err)
	require.True(t, exist)
	assert.Equal(t, strings.Repeat("é", 200), stored.Summary)
}

// Only an icon of the set may be stored, and an empty name is how a session drops its icon.
func TestConversationIconMustBeKnownAndCanBeCleared(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	session, err := CreateConversation(ctx, agent_model.ConversationKindServer, owner.ID, 0, "Named")
	require.NoError(t, err)

	assert.ErrorIs(t, SetConversationIcon(ctx, session, "octicon-not-in-the-set"), ErrConversationTitleInvalid{})

	icon := ConversationIcons()[0]
	require.NoError(t, SetConversationIcon(ctx, session, icon))
	assert.Equal(t, icon, session.Icon)

	require.NoError(t, SetConversationIcon(ctx, session, ""))
	assert.Empty(t, session.Icon)
}

func TestFolderNameIsRequiredAndNormalized(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	_, err := CreateConversationFolder(ctx, "   ", "")
	assert.ErrorIs(t, err, ErrConversationFolderInvalid{})

	_, err = CreateConversationFolder(ctx, "Work", "octicon-not-in-the-set")
	assert.ErrorIs(t, err, ErrConversationFolderInvalid{})

	folder, err := CreateConversationFolder(ctx, "  Work  ", ConversationFolderIcons()[0])
	require.NoError(t, err)
	assert.Equal(t, "Work", folder.Name)

	_, err = UpdateConversationFolder(ctx, folder.ID, "\t", "")
	assert.ErrorIs(t, err, ErrConversationFolderInvalid{})

	_, err = UpdateConversationFolder(ctx, folder.ID, "Work", "octicon-not-in-the-set")
	assert.ErrorIs(t, err, ErrConversationFolderInvalid{})

	updated, err := UpdateConversationFolder(ctx, folder.ID, strings.Repeat("é", 150), "")
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("é", 100), updated.Name)
}

func TestFolderDeletionMovesItsConversationsBackToTheRoot(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	folder, err := CreateConversationFolder(ctx, "Work", "")
	require.NoError(t, err)

	session, err := CreateConversation(ctx, agent_model.ConversationKindServer, owner.ID, 0, "Filed")
	require.NoError(t, err)
	require.NoError(t, SetConversationFolder(ctx, session, folder.ID))
	assert.Equal(t, folder.ID, session.FolderID)

	require.NoError(t, DeleteConversationFolder(ctx, folder.ID))

	stored, exist, err := db.GetByID[agent_model.Conversation](ctx, session.ID)
	require.NoError(t, err)
	require.True(t, exist)
	assert.Zero(t, stored.FolderID)

	_, err = GetConversationFolder(ctx, folder.ID)
	assert.ErrorIs(t, err, ErrConversationFolderNotFound{})
}

func TestAppendMessageRefusesBlankContentAndUnknownRole(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	session, err := CreateConversation(ctx, agent_model.ConversationKindServer, owner.ID, 0, "Named")
	require.NoError(t, err)

	_, err = AppendMessage(ctx, session.ID, agent_model.MessageRoleUser, "  \n ", "")
	assert.Error(t, err)

	_, err = AppendMessage(ctx, session.ID, "wizard", "hello", "")
	assert.ErrorContains(t, err, "unknown message role")

	for _, role := range []string{
		agent_model.MessageRoleUser,
		agent_model.MessageRoleAssistant,
		agent_model.MessageRoleSystem,
		agent_model.MessageRoleTool,
	} {
		message, err := AppendMessage(ctx, session.ID, role, "  hello  ", "")
		require.NoError(t, err)
		assert.Equal(t, role, message.Role)
		assert.Equal(t, "hello", message.Content)
	}
}

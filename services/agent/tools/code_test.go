package tools

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodeReadsTheRepositoryHistory(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	listed, err := listCommitsTool{}.Run(ctx, owner, session, `{}`)
	require.NoError(t, err)
	assert.Contains(t, listed, "Initial commit")

	commits, err := listCommitsTool{}.Run(ctx, owner, session, `{"path":"README.md"}`)
	require.NoError(t, err)
	assert.Contains(t, commits, "Initial commit")

	branches, err := listBranchesTool{}.Run(ctx, owner, session, `{}`)
	require.NoError(t, err)
	assert.Contains(t, branches, "master (the default branch)")
	assert.Contains(t, branches, "branch2")
}

func TestSearchFindsWhereThingsAreWritten(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	found, err := searchCodeTool{}.Run(ctx, owner, session, `{"keyword":"Description for repo1"}`)
	require.NoError(t, err)
	assert.Contains(t, found, "README.md")

	missing, err := searchCodeTool{}.Run(ctx, owner, session, `{"keyword":"quantum-widget-macro"}`)
	require.NoError(t, err)
	assert.Contains(t, missing, "Nothing in user2/repo1")
}

func TestCommitIsRefusedWhereItMustNotLand(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	// the default branch is not for an agent to write to
	refused, err := commitFilesTool{}.Run(ctx, owner, session, `{"branch":"master","message":"docs: x","files":[{"path":"NOTES.md","content":"x"}]}`)
	require.NoError(t, err)
	assert.Contains(t, refused, "default branch")

	// a call without the branch and message is answered rather than run
	missing, err := commitFilesTool{}.Run(ctx, owner, session, `{"branch":"agent/notes"}`)
	require.NoError(t, err)
	assert.Contains(t, missing, "The branch and message arguments are required.")
}

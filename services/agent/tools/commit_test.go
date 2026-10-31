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

// The branch tools refuse the default branch and refuse nothing else that a reader may undo.
func TestBranchToolsRefuseTheDefaultBranch(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	refused, err := createBranchTool{}.Run(ctx, owner, session, `{"branch":"master"}`)
	require.NoError(t, err)
	assert.Contains(t, refused, "choose another name for yours")

	deleted, err := deleteBranchTool{}.Run(ctx, owner, session, `{"branch":"master"}`)
	require.NoError(t, err)
	assert.Contains(t, deleted, "cannot be deleted")
}

// A patch is how a code change is committed: one file, the lines the reader asked to change.
func TestPatchToolAppliesADiffOnABranchOfItsOwn(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	refused, err := patchFileTool{}.Run(ctx, owner, session, `{"branch":"master","path":"README.md","diff":"x","message":"m"}`)
	require.NoError(t, err)
	assert.Contains(t, refused, "default branch")

	missing, err := patchFileTool{}.Run(ctx, owner, session, `{"branch":"patched","path":"README.md","message":"m"}`)
	require.NoError(t, err)
	assert.Contains(t, missing, "The branch, diff and message arguments are required.")
}

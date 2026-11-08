// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"

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

// A change goes to the branch it names: from that branch when it is already there, and from the
// base when the branch is still to be made.
func TestOldBranchForFollowsTheBranchState(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	gitRepo, err := git.OpenRepository(ctx, repo)
	require.NoError(t, err)
	defer gitRepo.Close()

	// branch2 is in the fixture's git repository, so a commit to it updates it
	assert.Equal(t, "branch2", oldBranchFor(ctx, gitRepo, "branch2", "master"))
	// one that is not there yet is made from the base
	assert.Equal(t, "master", oldBranchFor(ctx, gitRepo, "branch-that-is-not-there", "master"))
	assert.Equal(t, "branch1", oldBranchFor(ctx, gitRepo, "branch-that-is-not-there", "branch1"))
}

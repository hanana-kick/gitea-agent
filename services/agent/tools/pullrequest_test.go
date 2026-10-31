// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

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

// What a pull request changes is read from the branches it merges, file by file and patch by
// patch; an issue has nothing to change and says so.
func TestReadPullRequestReadsItsChanges(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	files, err := readPullRequestTool{}.Run(ctx, owner, session, `{"index":3}`)
	require.NoError(t, err)
	assert.Contains(t, files, "user2/repo1 #3")
	assert.Contains(t, files, "- `README.md` (+4 −1)")
	assert.Contains(t, files, "- `3` (deleted)")
	assert.Contains(t, files, "2 files changed, +4 −2")
	assert.Contains(t, files, "The changed files are on branch2") // where reading one of them starts

	// one path of the change reads as a patch: hunks and their added lines
	patch, err := readPullRequestTool{}.Run(ctx, owner, session, `{"index":3,"path":"README.md"}`)
	require.NoError(t, err)
	assert.Contains(t, patch, "`README.md`")
	assert.Contains(t, patch, "@@")
	assert.Contains(t, patch, "\n+And change for branch2") // what the branch adds
	assert.Contains(t, patch, "\n-Description for repo1")  // and what it takes away

	// a path the change does not touch is answered, not failed
	untouched, err := readPullRequestTool{}.Run(ctx, owner, session, `{"index":3,"path":"go.mod"}`)
	require.NoError(t, err)
	assert.Contains(t, untouched, "does not change")

	// an issue is not a change: the answer says which tool reads it
	issue, err := readPullRequestTool{}.Run(ctx, owner, session, `{"index":1}`)
	require.NoError(t, err)
	assert.Contains(t, issue, "not a pull request")
	assert.Contains(t, issue, readIssueToolName)

	// a number nothing has, and a call without a number
	missing, err := readPullRequestTool{}.Run(ctx, owner, session, `{"index":999}`)
	require.NoError(t, err)
	assert.Contains(t, missing, "There is no issue or pull request with that number")

	none, err := readPullRequestTool{}.Run(ctx, owner, session, `{}`)
	require.NoError(t, err)
	assert.Contains(t, none, "The index argument is required.")
}

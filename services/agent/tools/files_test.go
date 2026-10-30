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

// The file tools read the repository of the session, with the rights of the user who asked:
// the model is told why it cannot read something instead of being handed a failure.
func TestFileToolsReadTheSessionRepository(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	listed, err := listFilesTool{}.Run(ctx, owner, session, `{}`)
	require.NoError(t, err)
	assert.Contains(t, listed, "README.md")

	read, err := readFileTool{}.Run(ctx, owner, session, `{"path":"README.md"}`)
	require.NoError(t, err)
	assert.Contains(t, read, "`README.md` at master")
	assert.Contains(t, read, "README", "the file's own text comes back with it")

	// a path nothing stands at is an answer, not a failure: the model can try another one
	missing, err := readFileTool{}.Run(ctx, owner, session, `{"path":"docs/nope.md"}`)
	require.NoError(t, err)
	assert.Contains(t, missing, "There is nothing at")

	// a directory is not a file, and the answer says what to read it with instead
	directory, err := readFileTool{}.Run(ctx, owner, session, `{"path":"."}`)
	require.NoError(t, err)
	assert.Contains(t, directory, "is a directory")
	assert.Contains(t, directory, listFilesToolName)

	// a session that is not bound to a repository has nothing to read
	unbound, err := listFilesTool{}.Run(ctx, owner, Session{Kind: agent_model.ConversationKindServer}, `{}`)
	require.NoError(t, err)
	assert.Contains(t, unbound, "not bound to a repository")
}

// A user who may not read the repository reads nothing through the agent either.
func TestFileToolsReadWithTheUsersRights(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 2}
	stranger := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})

	out, err := readFileTool{}.Run(ctx, stranger, session, `{"path":"README.md"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "You cannot read the code of this repository.")
}

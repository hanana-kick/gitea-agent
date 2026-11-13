// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"html/template"
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

// The search tool answers from what the instance indexed when it can, and from the files when it
// cannot: an instance that runs no index, and a search narrowed to a path, read the files.
func TestSearchCodeReadsTheFilesWhenNoIndexAnswers(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	assert.False(t, indexedSearch(ctx, ""), "an instance without a code index answered from one")
	assert.False(t, indexedSearch(ctx, "services/"), "a search narrowed to a path was answered from the index")

	found, err := searchCodeTool{}.Run(ctx, owner, session, `{"keyword":"Description"}`)
	require.NoError(t, err)
	assert.Contains(t, found, "README.md:3:", "the line the word is on is not named")
	assert.Contains(t, found, "Description for repo1")

	nothing, err := searchCodeTool{}.Run(ctx, owner, session, `{"keyword":"surely-not-in-this-fixture"}`)
	require.NoError(t, err)
	assert.Contains(t, nothing, "Nothing in")
}

// What an index marks a match with is markup, and the model is handed the line itself.
func TestSearchLineTextDropsTheIndexMarkup(t *testing.T) {
	assert.Equal(t, "const drift = 1800", searchLineText(template.HTML(`const <span class="ui label">drift</span> = 1800`)))
	assert.Equal(t, "a < b > c", searchLineText(template.HTML("a &lt; b &gt; c")))
}

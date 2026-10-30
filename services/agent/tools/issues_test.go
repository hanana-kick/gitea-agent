// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A search stays inside the repository its session works in, and says what each hit is.
func TestSearchStaysInsideTheSessionRepository(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	bound := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	listed, err := searchIssuesTool{}.Run(ctx, owner, bound, `{}`)
	require.NoError(t, err)
	require.NotEqual(t, "No issues are visible here.", listed)
	for line := range strings.SplitSeq(listed, "\n") {
		assert.True(t, strings.HasPrefix(line, "user2/repo1 #"), line)
	}
	// an issue and a pull request are both open work, and each says which it is
	assert.Contains(t, listed, "(open")
	assert.Contains(t, listed, "pull request")

	closed, err := searchIssuesTool{}.Run(ctx, owner, bound, `{"state":"closed"}`)
	require.NoError(t, err)
	for line := range strings.SplitSeq(closed, "\n") {
		assert.Contains(t, line, "(closed", line)
	}

	// a session that spans the instance names the repository it means
	wide := Session{Kind: agent_model.ConversationKindServer}
	named, err := searchIssuesTool{}.Run(ctx, owner, wide, `{"repository":"user2/repo1"}`)
	require.NoError(t, err)
	assert.Contains(t, named, "user2/repo1 #")

	unknown, err := searchIssuesTool{}.Run(ctx, owner, wide, `{"repository":"user2/nope"}`)
	require.NoError(t, err)
	assert.Contains(t, unknown, "There is no repository user2/nope here.")

	// a repository session cannot be sent outside the repository it works in
	outside, err := searchIssuesTool{}.Run(ctx, owner, bound, `{"repository":"user2/repo2"}`)
	require.NoError(t, err)
	assert.Contains(t, outside, "can only look at that repository")

	// a search can ask for one kind of thing, which is what a question about pull requests means
	pulls, err := searchIssuesTool{}.Run(ctx, owner, bound, `{"kind":"pull request"}`)
	require.NoError(t, err)
	require.NotEqual(t, "No issues are visible here.", pulls)
	for line := range strings.SplitSeq(pulls, "\n") {
		assert.Contains(t, line, "pull request", line)
	}
	issuesOnly, err := searchIssuesTool{}.Run(ctx, owner, bound, `{"kind":"issue"}`)
	require.NoError(t, err)
	for line := range strings.SplitSeq(issuesOnly, "\n") {
		assert.NotContains(t, line, "pull request", line)
	}

	// the state has to be one of the three words the tool knows
	bad, err := searchIssuesTool{}.Run(ctx, owner, bound, `{"state":"merged"}`)
	require.NoError(t, err)
	assert.Contains(t, bad, `"open", "closed" or "all"`)

	unknownKind, err := searchIssuesTool{}.Run(ctx, owner, bound, `{"kind":"discussion"}`)
	require.NoError(t, err)
	assert.Contains(t, unknownKind, `"issue", "pull request"`)
}

// Reading one thing uses the number the search showed, not a database id: a pull request is
// read by its own number and says where it merges.
func TestReadIssueUsesTheNumberTheSearchShows(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	issue, err := readIssueTool{}.Run(ctx, owner, session, `{"index":1}`)
	require.NoError(t, err)
	assert.Contains(t, issue, "user2/repo1 #1")
	assert.NotContains(t, issue, "merges")

	pull, err := readIssueTool{}.Run(ctx, owner, session, `{"index":2}`)
	require.NoError(t, err)
	assert.Contains(t, pull, "user2/repo1 #2")
	assert.Contains(t, pull, "pull request")
	assert.Contains(t, pull, "branch1")
	assert.Contains(t, pull, "master")

	missing, err := readIssueTool{}.Run(ctx, owner, session, `{"index":999}`)
	require.NoError(t, err)
	assert.Contains(t, missing, "There is no issue or pull request with that number in user2/repo1.")

	none, err := readIssueTool{}.Run(ctx, owner, session, `{}`)
	require.NoError(t, err)
	assert.Contains(t, none, "The index argument is required.")
}

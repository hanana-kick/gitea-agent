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

// Merging says why it cannot happen rather than leaving the model with a git error: a request
// without a number, a change that landed already, and a style the repository does not offer.
func TestMergePullRequestRefusesWhatItCannotMerge(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	none, err := mergePullRequestTool{}.Run(ctx, owner, session, `{}`)
	require.NoError(t, err)
	assert.Contains(t, none, "The index argument is required.")

	unknown, err := mergePullRequestTool{}.Run(ctx, owner, session, `{"index":999}`)
	require.NoError(t, err)
	assert.Contains(t, unknown, "There is no pull request user2/repo1 #999.")

	// the fixture's #2 is merged, so there is nothing left to land
	merged, err := mergePullRequestTool{}.Run(ctx, owner, session, `{"index":2}`)
	require.NoError(t, err)
	assert.Contains(t, merged, "is already merged")

	// a repository that does not offer a style says so, and says what it does offer
	elsewhere := Session{Kind: agent_model.ConversationKindRepository}
	style, err := mergePullRequestTool{}.Run(ctx, owner, elsewhere, `{"repository":"org3/repo3","index":1,"style":"squash"}`)
	require.NoError(t, err)
	assert.Contains(t, style, "does not allow the squash merge style")
	assert.Contains(t, style, `"merge"`)
}

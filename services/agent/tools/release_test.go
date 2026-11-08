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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Publishing a release says why it cannot: a request without a tag or a title, a tag that is
// already taken, and a tag a repository cannot hold.
func TestCreateReleaseRefusesWhatItCannotPublish(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	none, err := createReleaseTool{}.Run(ctx, owner, session, `{}`)
	require.NoError(t, err)
	assert.Contains(t, none, "The tag and title arguments are required.")

	titleOnly, err := createReleaseTool{}.Run(ctx, owner, session, `{"tag":"v9.9.9"}`)
	require.NoError(t, err)
	assert.Contains(t, titleOnly, "required")

	// the fixture's repository already publishes v1.1
	taken, err := createReleaseTool{}.Run(ctx, owner, session, `{"tag":"v1.1","title":"again"}`)
	require.NoError(t, err)
	assert.Contains(t, taken, "already has a release or tag named v1.1")
}

// Notes left out are written from the commits since the last release, which a repository that
// already published one cannot offer until that release is named.
func TestCreateReleaseWritesNotesOfAFollowUpRelease(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	published, err := createReleaseTool{}.Run(ctx, owner, session, `{"tag":"v9.0.0","title":"the next one"}`)
	require.NoError(t, err)
	assert.Contains(t, published, "v9.0.0")

	release, err := repo_model.GetRelease(ctx, 1, "v9.0.0")
	require.NoError(t, err)
	assert.Equal(t, "the next one", release.Title)
	assert.Equal(t, owner.ID, release.PublisherID)
}

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

// The CI tool reads the runs of the session's repository, and one run's jobs when asked for one.
func TestListRunsReadsTheRepositoryCITool(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	repository := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 4})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repository.OwnerID})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: repository.ID}

	listed, err := listRunsTool{}.Run(ctx, owner, session, `{}`)
	require.NoError(t, err)
	assert.Contains(t, listed, "artifact.yaml")
	assert.Contains(t, listed, "on master at ")

	// a run number asks what the run's jobs did
	jobs, err := listRunsTool{}.Run(ctx, owner, session, `{"run":187}`)
	require.NoError(t, err)
	assert.Contains(t, jobs, "#187")
	assert.Contains(t, jobs, "- ")

	// a number no run has is answered, not failed
	missing, err := listRunsTool{}.Run(ctx, owner, session, `{"run":987654}`)
	require.NoError(t, err)
	assert.Contains(t, missing, "There is no CI run #987654")

	// the status has to be one of the words the tool knows
	bad, err := listRunsTool{}.Run(ctx, owner, session, `{"status":"flaky"}`)
	require.NoError(t, err)
	assert.Contains(t, bad, `"success", "failure", "running", "waiting", "cancelled" or "all"`)
}

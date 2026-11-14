// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/services/agent/sandbox"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a run runs commands in is the same checkout for every command of that revision: what one of
// them left is there for the next, and a commit the run made is what the next one reads, which is
// what makes building on it worth anything.
func TestCommandWorkspaceFollowsWhatTheRunWrites(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	session := Session{Kind: agent_model.ConversationKindRepository, RepoID: repo.ID}
	// the fixture hooks exec the built binary with a config a unit test never has
	require.NoError(t, os.RemoveAll(filepath.Join(gitrepo.RepoLocalPath(repo), "hooks")))
	t.Cleanup(func() { sandbox.Forget(repo) })

	work, err := sandbox.Workspace(ctx, repo, "")
	require.NoError(t, err)
	// what a command left in the checkout is what the command after it finds
	build := filepath.Join(work.Dir, "build-artifact")
	require.NoError(t, os.WriteFile(build, []byte("built"), 0o644))

	again, err := sandbox.Workspace(ctx, repo, "")
	require.NoError(t, err)
	assert.Equal(t, work.Dir, again.Dir, "a second command was given a checkout of its own")
	_, err = os.Stat(filepath.Join(again.Dir, "build-artifact"))
	assert.NoError(t, err, "what the first command left was thrown away")

	// a commit the run makes is what the next command reads
	_, err = commitFilesTool{}.Run(ctx, owner, session, `{"branch":"workspace-probe","message":"docs: probe the workspace","files":[{"path":"probe.md","content":"the run wrote this\n"}]}`)
	require.NoError(t, err)

	followed, err := sandbox.Workspace(ctx, repo, "workspace-probe")
	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(followed.Dir, "probe.md"))
	require.NoError(t, err, "the commit the run made is not in the checkout the next command reads")
	assert.Equal(t, "the run wrote this\n", string(content))
}

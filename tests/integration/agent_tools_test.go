// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/url"
	"testing"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	agent_tools "gitea.dev/services/agent/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentCommitFilesTool(t *testing.T) {
	onGiteaRun(t, func(t *testing.T, u *url.URL) {
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
		session := agent_tools.Session{Kind: agent_model.ConversationKindRepository, RepoID: repo.ID}

		committed, err := agent_tools.CommitFilesTool{}.Run(t.Context(), user, session, `{"branch":"agent/notes","message":"docs: add notes","files":[{"path":"NOTES.md","content":"notes"}]}`)
		require.NoError(t, err)
		assert.Contains(t, committed, "Committed 1 file(s) to agent/notes")
		assert.Contains(t, committed, "Open a pull request from it")

		read, err := agent_tools.ReadFileTool{}.Run(t.Context(), user, session, `{"path":"NOTES.md","ref":"agent/notes"}`)
		require.NoError(t, err)
		assert.Contains(t, read, "notes")

		created, err := agent_tools.CreateBranchTool{}.Run(t.Context(), user, session, `{"branch":"agent/from-tool","from":"master"}`)
		require.NoError(t, err)
		assert.Contains(t, created, "Branch agent/from-tool created from master")
	})
}

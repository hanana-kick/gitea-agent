// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/unittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	unittest.MainTest(m)
}

// A call is checked against the whole session, not just its kind: the tools that read a
// session's repository are offered by the repository it works in.
func TestCheckReadsTheSessionItIsGiven(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	repository := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	tool, decision, err := Check(ctx, repository, "gitea_read_file")
	require.NoError(t, err)
	assert.NotNil(t, tool)
	assert.Equal(t, DecisionRun, decision)

	kindOnly := Session{Kind: agent_model.ConversationKindRepository}
	_, decision, err = Check(ctx, kindOnly, "gitea_read_file")
	require.NoError(t, err)
	assert.Equal(t, DecisionDenied, decision, "a session with no repository has no files to read")

	// the admin can switch a tool off, and that holds wherever it would be offered
	require.NoError(t, SetMode(ctx, "gitea_read_file", PermissionLLM, false))
	t.Cleanup(func() { _ = SetMode(ctx, "gitea_read_file", PermissionLLM, true) })
	_, decision, err = Check(ctx, repository, "gitea_read_file")
	require.NoError(t, err)
	assert.Equal(t, DecisionDenied, decision)

	// a tool that asks first is offered, and the call waits for its reader
	require.NoError(t, SetMode(ctx, "gitea_read_file", PermissionManual, true))
	_, decision, err = Check(ctx, repository, "gitea_read_file")
	require.NoError(t, err)
	assert.Equal(t, DecisionAsk, decision)

	// a name nothing answers to is denied, so a made-up tool costs the run nothing
	_, decision, err = Check(ctx, repository, "gitea_nonesuch")
	assert.Error(t, err)
	assert.Equal(t, DecisionDenied, decision)
}

// What a session is offered is what it was left enabled with, narrowed by where it works.
func TestAvailableNarrowsTheRegistryToTheSession(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	names := func(session Session) []string {
		list, err := Available(ctx, session)
		require.NoError(t, err)
		found := make([]string, 0, len(list))
		for _, tool := range list {
			found = append(found, tool.Name())
		}
		return found
	}

	repository := names(Session{Kind: agent_model.ConversationKindRepository, RepoID: 1})
	assert.Contains(t, repository, "gitea_read_file")
	assert.Contains(t, repository, "gitea_read_issue")
	// a repository session works inside one repository, so the list of them has nothing to say
	assert.NotContains(t, repository, "gitea_list_repos")

	server := names(Session{Kind: agent_model.ConversationKindServer})
	assert.Contains(t, server, "gitea_list_repos")
	// with no repository of its own, it has no files to read
	assert.NotContains(t, server, "gitea_read_file")
	assert.NotContains(t, server, "gitea_list_files")

	// nothing of the session's own scope reaches a session kind narrower than its own
	kindOnly := names(Session{Kind: "nonsense"})
	assert.NotContains(t, kindOnly, "gitea_read_file")
}

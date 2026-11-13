// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A repository leaves the rules it works by in a file, and a run of it reads them: the file the
// instance names is quoted into the prompt, a file that is not there says nothing, and a session
// that is not bound to a repository reads none of it.
func TestRepositoryInstructionsReachThePrompt(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	repository := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	original := setting.Agent.Instructions
	t.Cleanup(func() { setting.Agent.Instructions = original })

	// the fixture repository keeps its rules in README.md, which is a name an instance may set
	setting.Agent.Instructions = []string{"README.md"}
	instructions := repositoryInstructions(ctx, repository)
	assert.Contains(t, instructions, "user2/repo1", "the prompt does not say which repository it is about")
	assert.Contains(t, instructions, "### README.md")
	assert.Contains(t, instructions, "```", "the repository's own text is not quoted")

	// a file the repository does not have leaves the prompt as it was
	setting.Agent.Instructions = []string{"AGENTS.md"}
	assert.Empty(t, repositoryInstructions(ctx, repository))

	// and an instance that names nothing reads nothing
	setting.Agent.Instructions = nil
	assert.Empty(t, repositoryInstructions(ctx, repository))

	setting.Agent.Instructions = []string{"README.md"}
	assert.Empty(t, repositoryInstructions(ctx, &agent_model.Conversation{Kind: agent_model.ConversationKindServer}),
		"a session that is not bound to a repository read one")
}

// What a repository says stands in the system prompt of a run, before its history.
func TestRepositoryInstructionsArePartOfTheSystemPrompt(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	original := setting.Agent.Instructions
	t.Cleanup(func() { setting.Agent.Instructions = original })
	setting.Agent.Instructions = []string{"README.md"}

	messages := buildPrompt(context.Background(), &agent_model.Conversation{
		Kind: agent_model.ConversationKindRepository, RepoID: 1, Title: "a session",
	}, nil, nil, nil, nil)
	require.NotEmpty(t, messages)
	assert.Equal(t, "system", messages[0].Role)
	assert.Contains(t, messages[0].Content, "### README.md")
}

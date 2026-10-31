// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryIsStableAndComplete(t *testing.T) {
	list := Registry()
	names := make([]string, 0, len(list))
	for _, tool := range list {
		names = append(names, tool.Name())
		assert.NotEmpty(t, tool.Description(), "%s needs a description for the model", tool.Name())
	}
	assert.Equal(t, []string{
		"gitea_comment_issue", "gitea_commit_files", "gitea_list_actions_runs", "gitea_list_branches",
		"gitea_list_commits", "gitea_list_files", "gitea_list_repos", "gitea_read_commit", "gitea_read_file",
		"gitea_read_issue", "gitea_read_pull_request", "gitea_read_pull_request_reviews",
		"gitea_read_skill", "gitea_review_pull_request", "gitea_search_code", "gitea_search_issues",
	}, names)
}

func TestScopeAllowedFor(t *testing.T) {
	// the data of the caller is fair game wherever the agent works
	for _, kind := range []string{agent_model.ConversationKindResident, agent_model.ConversationKindServer, agent_model.ConversationKindRepository} {
		assert.True(t, ScopeUser.AllowedFor(kind), "user scope in %s", kind)
	}
	// instance-wide tools stay out of a repository session
	assert.True(t, ScopeServer.AllowedFor(agent_model.ConversationKindResident))
	assert.True(t, ScopeServer.AllowedFor(agent_model.ConversationKindServer))
	assert.False(t, ScopeServer.AllowedFor(agent_model.ConversationKindRepository))
	// one repository is reachable from a session that already spans the instance
	assert.True(t, ScopeRepository.AllowedFor(agent_model.ConversationKindRepository))
	assert.True(t, ScopeRepository.AllowedFor(agent_model.ConversationKindServer))
	assert.False(t, ScopeRepository.AllowedFor("nonsense"))
}

func TestDefinitionsDescribeArgumentsForTheModel(t *testing.T) {
	defs := Definitions([]Tool{searchIssuesTool{}, listReposTool{}})
	require.Len(t, defs, 2)

	first := defs[0]
	assert.Equal(t, "function", first.Type)
	assert.Equal(t, "gitea_search_issues", first.Function.Name)
	parameters, err := json.Marshal(first.Function.Parameters)
	require.NoError(t, err)
	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	require.NoError(t, json.Unmarshal(parameters, &schema))
	assert.Equal(t, "object", schema.Type)
	// the keyword may be left out: without one the tool lists what is there
	assert.Empty(t, schema.Required)
	assert.Equal(t, "string", schema.Properties["keyword"]["type"])
	assert.NotEmpty(t, schema.Properties["keyword"]["description"])

	// a tool that takes nothing asks for no properties and demands nothing
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{}}, defs[1].Function.Parameters)

	// the number has to arrive as a number, or the tool cannot decode it
	numbered := Definitions([]Tool{readIssueTool{}})[0].Function.Parameters
	numberedProperties, ok := numbered["properties"].(map[string]any)
	require.True(t, ok)
	index, ok := numberedProperties["index"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "integer", index["type"])
	assert.NotEmpty(t, index["description"])
	assert.Equal(t, []string{"index"}, numbered["required"]) // the repository may be left to the session
}

func TestListReposIsOfferedOnlyOutsideARepository(t *testing.T) {
	ctx := context.Background()
	// a repository session works inside one repository, so the user's other repositories are none of its business
	offered, err := listReposTool{}.Offered(ctx, Session{Kind: agent_model.ConversationKindRepository, RepoID: 3})
	require.NoError(t, err)
	assert.False(t, offered)

	for _, kind := range []string{agent_model.ConversationKindResident, agent_model.ConversationKindServer} {
		offered, err := listReposTool{}.Offered(ctx, Session{Kind: kind})
		require.NoError(t, err)
		assert.True(t, offered, "in %s", kind)
	}
}

func TestFileToolsFollowTheRepositoryOfTheSession(t *testing.T) {
	for _, tool := range []Tool{listFilesTool{}, readFileTool{}} {
		// a session bound to no repository names the one it means, so the tool reaches it
		assert.True(t, tool.Scope().AllowedFor(agent_model.ConversationKindRepository))
		assert.True(t, tool.Scope().AllowedFor(agent_model.ConversationKindServer))

		// the reach is the repository's, so a session that sees less cannot have it
		assert.False(t, tool.Scope().AllowedFor("nonsense"))
	}

	// every file tool says how a session that is not bound to a repository asks for one
	for _, tool := range []Tool{listFilesTool{}, readFileTool{}} {
		first := tool.Args()[0]
		assert.Equal(t, repositoryNameArg, first.Name)
		assert.True(t, first.Optional, "%s leaves the repository to the session it runs in", tool.Name())
		assert.NotEmpty(t, first.Description)
	}
}

func TestFileToolsArgueWithPathsAndRevisions(t *testing.T) {
	byName := map[string][]Arg{}
	for _, tool := range []Tool{listFilesTool{}, readFileTool{}} {
		byName[tool.Name()] = tool.Args()
	}
	// a listing starts at the root, so its path may be left out; reading needs one
	// a listing starts at the root, so its path may be left out; reading needs one
	assert.True(t, byName["gitea_list_files"][1].Optional)
	assert.False(t, byName["gitea_read_file"][1].Optional)
	// both read the default branch unless another revision is named
	for _, name := range []string{"gitea_list_files", "gitea_read_file"} {
		args := byName[name]
		require.Len(t, args, 3)
		assert.Equal(t, "ref", args[2].Name)
		assert.True(t, args[2].Optional, "%s reads the default branch by default", name)
	}

	defs := Definitions([]Tool{readFileTool{}})
	required, ok := defs[0].Function.Parameters["required"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"path"}, required) // the model must say what to read
}

func TestSizeTextSpeaksOfFiles(t *testing.T) {
	assert.Equal(t, "0 B", sizeText(0))
	assert.Equal(t, "1023 B", sizeText(1023))
	assert.Equal(t, "1.0 KB", sizeText(1024))
	assert.Equal(t, "8.0 KB", sizeText(toolOutputLimit))
	assert.Equal(t, "1.0 MB", sizeText(1024*1024))
}

func TestClipKeepsToolOutputInsideThePrompt(t *testing.T) {
	short := "two\nlines"
	assert.Equal(t, short, clip(short))

	long := strings.Repeat("x\n", toolOutputLimit)
	clipped := clip(long)
	assert.Less(t, len(clipped), len(long))
	assert.True(t, strings.HasSuffix(clipped, "\n[truncated]"), "the reader must see that the tool cut its answer")
	assert.LessOrEqual(t, len(clipped), toolOutputLimit+len("\n[truncated]"))
}

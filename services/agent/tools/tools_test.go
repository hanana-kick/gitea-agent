// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
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
	assert.Equal(t, []string{"gitea_list_repos", "gitea_read_issue", "gitea_search_issues"}, names)
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
	assert.Equal(t, []string{"keyword"}, schema.Required)
	assert.Equal(t, "string", schema.Properties["keyword"]["type"])
	assert.NotEmpty(t, schema.Properties["keyword"]["description"])

	// a tool that takes nothing asks for no properties and demands nothing
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{}}, defs[1].Function.Parameters)

	// an id argument has to arrive as a number, or the tool cannot decode it
	numbered := Definitions([]Tool{readIssueTool{}})[0].Function.Parameters
	numberedProperties, ok := numbered["properties"].(map[string]any)
	require.True(t, ok)
	issueID, ok := numberedProperties["issue_id"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "integer", issueID["type"])
	assert.NotEmpty(t, issueID["description"])
	assert.Equal(t, []string{"issue_id"}, numbered["required"])
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

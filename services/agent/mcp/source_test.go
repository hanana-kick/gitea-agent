// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	agent_tools "gitea.dev/services/agent/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// the specs read and write the instance's rows, so the test database is prepared for them
	unittest.MainTest(m)
}

func toolNamed(list []agent_tools.Tool, name string) agent_tools.Tool {
	for _, tool := range list {
		if tool.Name() == name {
			return tool
		}
	}
	return nil
}

// A tool of a server joins the ones the agent is offered: it reaches a session that works on the
// whole instance and not one bound to a repository, it waits for an administrator to switch it on,
// and the model reads the arguments the server described rather than a translation of them.
func TestServerToolsJoinTheAgentsTools(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	stub := &stubServer{t: t, wants: "a-secret"}
	server := stub.start()
	defer server.Close()

	// the engine names this source while the instance starts; a spec names it for itself
	agent_tools.SetRemote(Source{})
	t.Cleanup(func() { agent_tools.SetRemote(nil) })

	row, err := CreateServer(ctx, ServerOptions{Name: "Test Server", URL: server.URL, Token: "a-secret", Enabled: true})
	require.NoError(t, err)
	assert.Equal(t, "test-server", row.Slug)
	assert.NotContains(t, row.TokenEnc, "a-secret", "the token is stored in the clear")
	t.Cleanup(func() {
		_ = DeleteServer(context.Background(), row.ID)
		_, _ = db.GetEngine(context.Background()).Where("tool LIKE ?", "mcp__%").Delete(&agent_model.ToolPermission{})
	})

	instanceSession := agent_tools.Session{Kind: agent_model.ConversationKindServer}
	offered, err := agent_tools.Available(ctx, instanceSession)
	require.NoError(t, err)
	assert.Nil(t, toolNamed(offered, "mcp__test-server__echo"), "a tool nobody switched on was offered")

	// a tool of a server is offered switched off, and asking its reader, until an admin decides
	mode, enabled, err := agent_tools.ModeOf(ctx, "mcp__test-server__echo")
	require.NoError(t, err)
	assert.Equal(t, agent_tools.PermissionManual, mode)
	assert.False(t, enabled, "a tool of a server is offered before an admin looks at it")

	require.NoError(t, agent_tools.SetMode(ctx, "mcp__test-server__echo", agent_tools.PermissionManual, true))
	offered, err = agent_tools.Available(ctx, instanceSession)
	require.NoError(t, err)
	echo := toolNamed(offered, "mcp__test-server__echo")
	require.NotNil(t, echo, "the tool an admin switched on is not offered")
	assert.Contains(t, echo.Description(), "Test Server", "the model cannot tell which server a tool came from")

	// what the server said the tool takes is what the model is handed
	defs := agent_tools.Definitions([]agent_tools.Tool{echo})
	require.Len(t, defs, 1)
	properties, ok := defs[0].Function.Parameters["properties"].(map[string]any)
	require.True(t, ok, "the tool is described by our own arguments instead of the server's")
	assert.Contains(t, properties, "text")

	// a call asks its reader first, because nobody here vouched for what the server does
	_, decision, err := agent_tools.Check(ctx, instanceSession, "mcp__test-server__echo")
	require.NoError(t, err)
	assert.Equal(t, agent_tools.DecisionAsk, decision)

	// a session bound to one repository is not offered it: a server is called with the credential
	// an administrator gave it, which a repository session must not be able to spend
	repoSession := agent_tools.Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}
	offered, err = agent_tools.Available(ctx, repoSession)
	require.NoError(t, err)
	assert.Nil(t, toolNamed(offered, "mcp__test-server__echo"))

	// and calling it goes to the server
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	answer, err := echo.Run(ctx, owner, instanceSession, `{"text":"hi"}`)
	require.NoError(t, err)
	assert.Equal(t, "echo: hi", answer)

	// a tool the server says failed is something the model can read, not a failure of the run
	failed, err := echo.Run(ctx, owner, instanceSession, `{"text":"hi"}`)
	require.NoError(t, err)
	assert.Contains(t, failed, "echo: hi")
}

// A server an administrator marks as only reading is offered without asking a reader each time.
func TestAReadOnlyServerIsOfferedWithoutAsking(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	stub := &stubServer{t: t}
	server := stub.start()
	defer server.Close()

	// the engine names this source while the instance starts; a spec names it for itself
	agent_tools.SetRemote(Source{})
	t.Cleanup(func() { agent_tools.SetRemote(nil) })

	row, err := CreateServer(ctx, ServerOptions{Name: "Reading Server", URL: server.URL, ReadOnly: true, Enabled: true})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = DeleteServer(context.Background(), row.ID)
		_, _ = db.GetEngine(context.Background()).Where("tool LIKE ?", "mcp__%").Delete(&agent_model.ToolPermission{})
	})

	mode, enabled, err := agent_tools.ModeOf(ctx, "mcp__reading-server__echo")
	require.NoError(t, err)
	assert.Equal(t, agent_tools.PermissionLLM, mode, "a server that only reads is held to ask its reader")
	assert.True(t, enabled)

	session := agent_tools.Session{Kind: agent_model.ConversationKindServer}
	_, decision, err := agent_tools.Check(ctx, session, "mcp__reading-server__echo")
	require.NoError(t, err)
	assert.Equal(t, agent_tools.DecisionRun, decision)
}

// A server that cannot be stored as asked says why, and one that is not there is not stored.
func TestServerValidation(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	_, err := CreateServer(ctx, ServerOptions{Name: "", URL: "http://example.com/mcp"})
	assert.ErrorIs(t, err, ErrServerInvalid{})

	_, err = CreateServer(ctx, ServerOptions{Name: "Local", URL: "file:///etc/passwd"})
	assert.ErrorIs(t, err, ErrServerInvalid{}, "a server may only be reached over http")

	first, err := CreateServer(ctx, ServerOptions{Name: "Same Name", URL: "http://example.com/mcp", Enabled: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = DeleteServer(context.Background(), first.ID) })

	_, err = CreateServer(ctx, ServerOptions{Name: "same name", URL: "http://example.com/mcp"})
	assert.ErrorIs(t, err, ErrServerInvalid{}, "two servers of the same name are not told apart")

	// a second server of the name that yields the same slug is numbered, not refused
	second, err := CreateServer(ctx, ServerOptions{Name: "Same-Name", URL: "http://example.com/mcp", Enabled: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = DeleteServer(context.Background(), second.ID) })
	assert.Equal(t, "same-name-2", second.Slug)
}

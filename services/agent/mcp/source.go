// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
	agent_tools "gitea.dev/services/agent/tools"
)

// prefix names a tool of a server, so it cannot be taken for one of ours and two servers may offer
// a tool of the same name.
const prefix = "mcp__"

// toolLimit is how much of what a remote tool answered reaches the model.
const toolLimit = 8 * 1024

// cacheLife is how long what a server offers is kept: a run asks for its tools every round, and a
// server is not asked again for a list it just gave.
const cacheLife = time.Minute

// Source is where the agent's remote tools come from. A remote tool is offered to the sessions
// that reach the whole instance rather than one repository: a server is called with the credential
// an administrator gave it, so a repository session must not be able to spend it.
type Source struct{}

// toolsKey is what a cached list of one server is filed under.
type toolsKey struct {
	serverID int64
	url      string
}

type cachedTools struct {
	tools []Tool
	at    time.Time
}

var (
	cacheMu sync.Mutex
	cache   = map[toolsKey]cachedTools{}
)

// Forget drops what was cached for a server, so an edit to it is read at once.
func Forget(serverID int64) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	for key := range cache {
		if key.serverID == serverID {
			delete(cache, key)
		}
	}
}

// ListTools returns the tools of every enabled server.
func (Source) ListTools(ctx context.Context, session agent_tools.Session) ([]agent_tools.Tool, error) {
	switch session.Kind {
	case agent_model.ConversationKindResident, agent_model.ConversationKindServer:
	default:
		// a session bound to one repository reaches no server: what it may not call, it is not
		// told about either
		return nil, nil
	}
	servers, err := EnabledServers(ctx)
	if err != nil {
		return nil, err
	}
	tools := make([]agent_tools.Tool, 0, len(servers)*4)
	for _, server := range servers {
		offered, err := toolsOf(ctx, server)
		if err != nil {
			// a server that cannot be reached leaves the run with the tools it has
			continue
		}
		for _, tool := range offered {
			tools = append(tools, remoteTool{server: server, tool: tool})
		}
	}
	return tools, nil
}

// FindTool returns one tool of one server by the name a model called.
func (Source) FindTool(name string) (agent_tools.Tool, bool) {
	slug, toolName, ok := splitName(name)
	if !ok {
		return nil, false
	}
	servers, err := EnabledServers(context.Background())
	if err != nil {
		return nil, false
	}
	for _, server := range servers {
		if server.Slug != slug {
			continue
		}
		offered, err := toolsOf(context.Background(), server)
		if err != nil {
			return nil, false
		}
		for _, tool := range offered {
			if tool.Name == toolName {
				return remoteTool{server: server, tool: tool}, true
			}
		}
		return nil, false
	}
	return nil, false
}

// EnabledServers reads the servers a run may call.
func EnabledServers(ctx context.Context) ([]*agent_model.MCPServer, error) {
	servers, _, err := agent_model.FindMCPServers(ctx, agent_model.FindMCPServersOptions{
		ListOptions: db.ListOptionsAll,
		EnabledOnly: true,
	})
	return servers, err
}

// toolsOf returns what one server offers, asking it again only when what was read is old.
func toolsOf(ctx context.Context, server *agent_model.MCPServer) ([]Tool, error) {
	key := toolsKey{serverID: server.ID, url: server.URL}
	cacheMu.Lock()
	entry, ok := cache[key]
	cacheMu.Unlock()
	if ok && time.Since(entry.at) < cacheLife {
		return entry.tools, nil
	}

	client, err := clientFor(server)
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithTimeout(ctx, requestLimit)
	defer cancel()
	if err := client.Initialize(readCtx); err != nil {
		return nil, err
	}
	tools, err := client.ListTools(readCtx)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	cache[key] = cachedTools{tools: tools, at: time.Now()}
	cacheMu.Unlock()
	return tools, nil
}

// clientFor prepares a client for a server, with the token it was given when it has one.
func clientFor(server *agent_model.MCPServer) (*Client, error) {
	token := ""
	if server.TokenEnc != "" {
		value, err := secret.DecryptSecret(setting.SecretKey, server.TokenEnc)
		if err != nil {
			return nil, fmt.Errorf("the token of %s cannot be read: %w", server.Name, err)
		}
		token = value
	}
	return NewClient(server.URL, token), nil
}

// ToolName is what a tool of a server is called: the server first, the tool after it.
func ToolName(slug, tool string) string {
	return prefix + slug + "__" + tool
}

// splitName reads a tool name back into the server it came from and the tool it names.
func splitName(name string) (slug, tool string, ok bool) {
	rest, found := strings.CutPrefix(name, prefix)
	if !found {
		return "", "", false
	}
	slug, tool, found = strings.Cut(rest, "__")
	if !found || slug == "" || tool == "" {
		return "", "", false
	}
	return slug, tool, true
}

// remoteTool is one tool of one server, as the agent sees it.
type remoteTool struct {
	server *agent_model.MCPServer
	tool   Tool
}

func (r remoteTool) Name() string { return ToolName(r.server.Slug, r.tool.Name) }

func (r remoteTool) Description() string {
	description := strings.TrimSpace(r.tool.Description)
	if title := strings.TrimSpace(r.tool.Title); title != "" && !strings.Contains(description, title) {
		description = title + ". " + description
	}
	if description == "" {
		description = "A tool of the server " + r.server.Name
	}
	return strings.TrimSpace(description + " (from the server " + r.server.Name + ")")
}

// Scope is the whole instance: a server is called with the credential an administrator gave it,
// and a session bound to one repository must not be able to spend it.
func (r remoteTool) Scope() agent_tools.Scope { return agent_tools.ScopeServer }

// Args is empty: the server described this tool's arguments, and the model is handed them.
func (r remoteTool) Args() []agent_tools.Arg { return nil }

// ToolSchema is what the server said this tool takes.
func (r remoteTool) ToolSchema() map[string]any {
	if len(r.tool.InputSchema) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return r.tool.InputSchema
}

// Writes reports whether calling this tool changes something. Nothing here knows what a server
// does, so a server is read as one that writes unless an administrator says it only reads: what
// nobody vouched for asks its reader first.
func (r remoteTool) Writes() bool { return !r.server.ReadOnly }

// Run calls the tool and returns what it answered, capped.
func (r remoteTool) Run(ctx context.Context, _ *user_model.User, _ agent_tools.Session, args string) (string, error) {
	client, err := clientFor(r.server)
	if err != nil {
		return "", err
	}
	if err := client.Initialize(ctx); err != nil {
		return "", err
	}
	answer, err := client.CallTool(ctx, r.tool.Name, args)
	if err != nil {
		// a tool that failed is something the model can act on, not a failure of ours
		return "The tool " + r.tool.Name + " of " + r.server.Name + " failed: " + err.Error(), nil
	}
	if len(answer) > toolLimit {
		answer = answer[:toolLimit] + "\n… (the answer is longer than this run reads)"
	}
	return answer, nil
}

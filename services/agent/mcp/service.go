// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
)

// ServerOptions is what an administrator edits of a server.
type ServerOptions struct {
	Name        string
	URL         string
	Description string
	// Token is the bearer token the server wants. Empty keeps the one that is stored.
	Token string
	// ClearToken takes the stored token away, for a server that no longer wants one.
	ClearToken bool
	// ReadOnly says the tools of this server only read, so they are offered without asking a
	// reader each time.
	ReadOnly bool
	Enabled  bool
}

// ErrServerInvalid is what a server that cannot be stored as asked is reported as.
type ErrServerInvalid struct{ Reason string }

func (err ErrServerInvalid) Error() string { return err.Reason }

// Is reports whether target is an ErrServerInvalid, whatever it says.
func (err ErrServerInvalid) Is(target error) bool {
	_, ok := target.(ErrServerInvalid)
	return ok
}

// Server is one server as the instance stores it.
type Server = agent_model.MCPServer

// GetServer reads one server by ID.
func GetServer(ctx context.Context, id int64) (*agent_model.MCPServer, error) {
	return agent_model.GetMCPServerByID(ctx, id)
}

// ListServers returns one page of servers and the total number of matches.
func ListServers(ctx context.Context, keyword string, page, pageSize int) ([]*agent_model.MCPServer, int64, error) {
	return agent_model.FindMCPServers(ctx, agent_model.FindMCPServersOptions{
		ListOptions: db.ListOptions{Page: page, PageSize: pageSize},
		Keyword:     keyword,
	})
}

// CreateServer stores a server an instance may call.
func CreateServer(ctx context.Context, opts ServerOptions) (*agent_model.MCPServer, error) {
	server, err := buildServer(ctx, 0, opts)
	if err != nil {
		return nil, err
	}
	if err := db.Insert(ctx, server); err != nil {
		return nil, fmt.Errorf("store the MCP server: %w", err)
	}
	return server, nil
}

// UpdateServer replaces the fields of a server.
func UpdateServer(ctx context.Context, id int64, opts ServerOptions) (*agent_model.MCPServer, error) {
	existing, err := agent_model.GetMCPServerByID(ctx, id)
	if err != nil {
		return nil, err
	}
	updated, err := buildServer(ctx, id, opts)
	if err != nil {
		return nil, err
	}
	updated.ID = existing.ID
	if opts.Token == "" && !opts.ClearToken {
		updated.TokenEnc = existing.TokenEnc
	}
	if _, err := db.GetEngine(ctx).ID(updated.ID).AllCols().Update(updated); err != nil {
		return nil, fmt.Errorf("update the MCP server: %w", err)
	}
	Forget(updated.ID)
	return updated, nil
}

// DeleteServer takes a server away, and stops offering the tools it had.
func DeleteServer(ctx context.Context, id int64) error {
	if _, err := db.DeleteByID[agent_model.MCPServer](ctx, id); err != nil {
		return fmt.Errorf("delete the MCP server: %w", err)
	}
	Forget(id)
	return nil
}

// ServerTools reads what a server offers, for an administrator who has just added it.
func ServerTools(ctx context.Context, id int64) ([]Tool, error) {
	server, err := agent_model.GetMCPServerByID(ctx, id)
	if err != nil {
		return nil, err
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
	cache[toolsKey{serverID: server.ID, url: server.URL}] = cachedTools{tools: tools, at: timeutil.TimeStampNow().AsTime()}
	cacheMu.Unlock()
	return tools, nil
}

// buildServer validates what an administrator submitted and returns the row to store.
func buildServer(ctx context.Context, exceptID int64, opts ServerOptions) (*agent_model.MCPServer, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return nil, ErrServerInvalid{Reason: "a server needs a name"}
	}
	if len(name) > 100 {
		return nil, ErrServerInvalid{Reason: "the server name is too long"}
	}
	address := strings.TrimSpace(opts.URL)
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrServerInvalid{Reason: "a server needs an http or https address"}
	}

	existing, _, err := agent_model.FindMCPServers(ctx, agent_model.FindMCPServersOptions{ListOptions: db.ListOptionsAll})
	if err != nil {
		return nil, err
	}
	for _, server := range existing {
		if server.ID != exceptID && strings.EqualFold(server.Name, name) {
			return nil, ErrServerInvalid{Reason: fmt.Sprintf("server %q already exists", name)}
		}
	}
	slug := agent_model.MCPServerSlugFromName(name)
	if slug == "" {
		return nil, ErrServerInvalid{Reason: "the server name needs a letter or a digit in it"}
	}
	taken := make(map[string]bool, len(existing))
	for _, server := range existing {
		if server.ID != exceptID {
			taken[server.Slug] = true
		}
	}
	if taken[slug] {
		// two servers may be called the same thing in two places; the second one is numbered
		for n := 2; ; n++ {
			candidate := fmt.Sprintf("%s-%d", slug, n)
			if !taken[candidate] {
				slug = candidate
				break
			}
		}
	}

	server := &agent_model.MCPServer{
		Name:        name,
		Slug:        slug,
		URL:         address,
		Description: strings.TrimSpace(opts.Description),
		ReadOnly:    opts.ReadOnly,
		Enabled:     opts.Enabled,
	}
	if token := strings.TrimSpace(opts.Token); token != "" {
		encrypted, err := secret.EncryptSecret(setting.SecretKey, token)
		if err != nil {
			return nil, fmt.Errorf("encrypt the server token: %w", err)
		}
		server.TokenEnc = encrypted
	}
	return server, nil
}

// ServerFromOptions is what a form submits, as the options a service takes.
func ServerFromOptions(name, address, description, token string, clearToken, readOnly, enabled bool) ServerOptions {
	return ServerOptions{
		Name: name, URL: address, Description: description, Token: token,
		ClearToken: clearToken, ReadOnly: readOnly, Enabled: enabled,
	}
}

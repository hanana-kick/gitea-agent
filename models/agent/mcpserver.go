// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"strings"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"

	"xorm.io/builder"
)

// MCPServer is one Model Context Protocol server the agent may call: its tools join the ones the
// agent is offered, under a name that keeps which server they came from. A server is reached over
// HTTP with a bearer token, which is kept encrypted and never handed back to a client.
type MCPServer struct {
	ID int64 `xorm:"pk autoincr"`
	// Name is what an admin calls the server, and Slug the part of a tool name that says which
	// server it came from: two servers may offer a tool of the same name.
	Name        string `xorm:"UNIQUE NOT NULL"`
	Slug        string `xorm:"UNIQUE NOT NULL"`
	URL         string `xorm:"VARCHAR(255) NOT NULL"`
	Description string `xorm:"TEXT"`
	// TokenEnc holds the bearer token the server wants, encrypted, when it wants one.
	TokenEnc string `xorm:"TEXT"`
	// ReadOnly says the tools of this server only read. Such a tool is offered as soon as an
	// admin switches it on; anything else asks its reader before it runs, like a write of ours.
	ReadOnly bool `xorm:"NOT NULL DEFAULT false"`
	Enabled  bool `xorm:"NOT NULL DEFAULT true"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (MCPServer) TableName() string {
	return "agent_mcp_server"
}

// MCPServerSlugFromName derives the part of a tool name that says which server it came from.
func MCPServerSlugFromName(name string) string {
	slug := strings.Builder{}
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			slug.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			if slug.Len() > 0 && !strings.HasSuffix(slug.String(), "-") {
				slug.WriteRune('-')
			}
		}
	}
	return strings.Trim(slug.String(), "-")
}

// FindMCPServersOptions describes which servers to read.
type FindMCPServersOptions struct {
	db.ListOptions
	// EnabledOnly narrows the answer to the servers a run may call.
	EnabledOnly bool
}

func (opts FindMCPServersOptions) ToConds() builder.Cond {
	cond := builder.NewCond()
	if opts.EnabledOnly {
		cond = cond.And(builder.Eq{"enabled": true})
	}
	return cond
}

func (opts FindMCPServersOptions) ToOrders() string {
	return "name"
}

// ErrMCPServerNotFound is returned when a server is not there.
type ErrMCPServerNotFound struct{ ID int64 }

func (err ErrMCPServerNotFound) Error() string { return "MCP server not found" }

// Is reports whether target is an ErrMCPServerNotFound, whatever its ID.
func (err ErrMCPServerNotFound) Is(target error) bool {
	_, ok := target.(ErrMCPServerNotFound)
	return ok
}

// FindMCPServers returns one page of servers and the total number of matches.
func FindMCPServers(ctx context.Context, opts FindMCPServersOptions) ([]*MCPServer, int64, error) {
	return db.FindAndCount[MCPServer](ctx, opts)
}

// GetMCPServerByID returns a server by ID.
func GetMCPServerByID(ctx context.Context, id int64) (*MCPServer, error) {
	server, exists, err := db.GetByID[MCPServer](ctx, id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrMCPServerNotFound{ID: id}
	}
	return server, nil
}

func init() {
	db.RegisterModel(new(MCPServer))
}

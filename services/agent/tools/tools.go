// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"slices"
	"strings"

	agent_model "gitea.dev/models/agent"
	user_model "gitea.dev/models/user"
	llm "gitea.dev/services/agent/llm"
)

// Scope is the reach a tool needs: it decides which session kinds may call it and what
// the tool is allowed to read.
type Scope string

const (
	// ScopeUser is the data of the calling user only.
	ScopeUser Scope = "user"
	// ScopeServer is instance-wide data, admins only.
	ScopeServer Scope = "server"
	// ScopeRepository is repository-scoped data.
	ScopeRepository Scope = "repository"
)

// reach ranks how far a scope looks. A session may use the tools of its own scope and of
// every narrower one, so a repository session can still read what its user can see.
func (s Scope) reach() int {
	switch s {
	case ScopeServer:
		return 2
	case ScopeRepository:
		return 1
	}
	return 0
}

// reachOf ranks what a session kind may reach: the resident and a server session see the
// whole instance, a repository session sees one repository.
func reachOf(sessionKind string) int {
	switch sessionKind {
	case agent_model.ConversationKindResident, agent_model.ConversationKindServer:
		return 2
	case agent_model.ConversationKindRepository:
		return 1
	}
	return 0
}

// AllowedFor reports whether a session kind may use a tool of this scope.
func (s Scope) AllowedFor(sessionKind string) bool {
	return s.reach() <= reachOf(sessionKind)
}

// Arg is one argument of a tool, described for the model.
type Arg struct {
	Name        string
	Description string
	// Type is the JSON type of the argument; "string" when empty.
	Type string
	// Optional marks an argument the model may leave out, so the schema does not demand it.
	Optional bool
}

// Session is what a tool may know about the session it runs in. Most tools only read what
// the calling user may see, but a tool that reaches further needs the session it is in.
type Session struct {
	Kind string
	// RepoID is the repository a repository session works inside, so its tools stay there; 0 for a session that is not bound to one.
	RepoID int64
}

// Tool is one capability the agent may call.
type Tool interface {
	Name() string
	// Description is written for the model: what it does and when to use it.
	Description() string
	Scope() Scope
	// Args returns the arguments in the order they are shown to the model.
	Args() []Arg
	// Run executes the call as the calling user, so every read stays inside what that
	// user may see.
	Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error)
}

// Conditional is a tool that is offered only when the session can use it, so a tool with
// nothing behind it costs the prompt nothing.
type Conditional interface {
	Offered(ctx context.Context, session Session) (bool, error)
}

// Permission modes of a tool.
const (
	// PermissionLLM lets the model decide when to call (default).
	PermissionLLM = "llm"
	// PermissionManual asks the user for approval on every call.
	PermissionManual = "manual"
	// PermissionAllowAll always allows.
	PermissionAllowAll = "allow_all"
)

// Registry returns every tool sorted by name; callers filter by scope and permission.
func Registry() []Tool {
	list := append([]Tool{}, GiteaTools{}.Registry()...)
	slices.SortFunc(list, func(a, b Tool) int { return strings.Compare(a.Name(), b.Name()) })
	return list
}

// Find returns a registered tool by name.
func Find(name string) (Tool, bool) {
	for _, tool := range Registry() {
		if tool.Name() == name {
			return tool, true
		}
	}
	return nil, false
}

// Definitions renders tools the way an OpenAI-compatible model expects to read them.
func Definitions(list []Tool) []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(list))
	for _, tool := range list {
		args := tool.Args()
		properties := make(map[string]any, len(args))
		required := make([]string, 0, len(args))
		for _, arg := range args {
			kind := arg.Type
			if kind == "" {
				kind = "string"
			}
			properties[arg.Name] = map[string]any{"type": kind, "description": arg.Description}
			if !arg.Optional {
				required = append(required, arg.Name)
			}
		}
		parameters := map[string]any{"type": "object", "properties": properties}
		if len(required) > 0 {
			parameters["required"] = required
		}
		defs = append(defs, llm.ToolDef{Type: "function", Function: llm.ToolFunction{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  parameters,
		}})
	}
	return defs
}

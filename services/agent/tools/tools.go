// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"encoding/json"

	user_model "gitea.dev/models/user"
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

// AllowedFor reports whether a session kind may use the tool: the session kind mirrors
// the scope naming (user sessions map to ScopeUser, server sessions to ScopeServer, and
// repository sessions to ScopeRepository).
func (s Scope) AllowedFor(sessionKind string) bool {
	return s == Scope(sessionKind)
}

// Tool is one capability the agent may call.
type Tool interface {
	Name() string
	// Description is written for the model: what it does and when to use it.
	Description() string
	Scope() Scope
	// Args returns the JSON-schema-style argument description shown to the model.
	Args() map[string]string
	// Run executes the call as the calling user, so every read stays inside what that
	// user may see.
	Run(ctx context.Context, doer *user_model.User, args json.RawMessage) (string, error)
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
	return []Tool{}
}

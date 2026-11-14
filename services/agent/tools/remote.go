// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import "context"

// Remote is where tools the instance does not ship with come from: an administrator adds a server,
// and the tools it offers join the ones a session may call. They are narrowed by the same rules as
// the tools of ours — what they need, whether the session can use them, and the permission an
// administrator set — so a remote tool is only unlike ours in where its call goes.
type Remote interface {
	// ListTools returns the tools a session may be offered.
	ListTools(ctx context.Context, session Session) ([]Tool, error)
	// FindTool returns one of them by the name a model called.
	FindTool(name string) (Tool, bool)
}

// Schema is a tool that writes its own arguments schema, as the server offering it described them:
// the model reads what the server said rather than a translation of it.
type Schema interface {
	ToolSchema() map[string]any
}

// remote is set once, while the instance starts, and read by every session after that.
var remote Remote

// SetRemote names where remote tools come from.
func SetRemote(source Remote) {
	remote = source
}

// remoteToolsFor returns what the remote source offers this session. A source that cannot answer
// leaves the session with the tools of ours, which is what it would have had anyway.
func remoteToolsFor(ctx context.Context, session Session) []Tool {
	if remote == nil {
		return nil
	}
	list, err := remote.ListTools(ctx, session)
	if err != nil {
		return nil
	}
	return list
}

// RemoteTools returns what the remote source offers, whatever the permissions say: the page that
// switches tools on has to show what is there before an administrator decides.
func RemoteTools(ctx context.Context, session Session) []Tool {
	return remoteToolsFor(ctx, session)
}

// findRemote returns a remote tool by name, for a call that is not one of ours.
func findRemote(name string) (Tool, bool) {
	if remote == nil {
		return nil, false
	}
	return remote.FindTool(name)
}

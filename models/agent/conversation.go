// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// Conversation kinds. They differ in scope, not in mechanics.
const (
	// ConversationKindResident is the single always-on agent that looks after the instance.
	ConversationKindResident = "resident"
	// ConversationKindServer is a throwaway agent for server-wide work, kept apart from the
	// resident one so experiments do not leak into it.
	ConversationKindServer = "server"
	// ConversationKindRepository is bound to one repository: code changes, pull requests, CI.
	ConversationKindRepository = "repository"
)

// IsValidConversationKind reports whether kind names a known session scope.
func IsValidConversationKind(kind string) bool {
	switch kind {
	case ConversationKindResident, ConversationKindServer, ConversationKindRepository:
		return true
	}
	return false
}

// Conversation is one agent chat session.
type Conversation struct {
	ID     int64  `xorm:"pk autoincr"`
	Kind   string `xorm:"VARCHAR(16) NOT NULL"`
	RepoID int64  `xorm:"INDEX NOT NULL DEFAULT 0"` // only set for repository sessions
	Title  string `xorm:"VARCHAR(100) NOT NULL"`
	// Summary describes the recent work of the session. The resident session keeps the name
	// Pod, so its generated name is shown as this summary instead.
	Summary string `xorm:"VARCHAR(200) NOT NULL DEFAULT ''"`
	// ModelRef is the model this session answers with: "group:<slug>" or
	// "provider:<id>/<model>". Empty means the session has no model yet.
	ModelRef string `xorm:"VARCHAR(160) NOT NULL DEFAULT ''"`
	// Icon is an icon name of the icon set, or empty for no icon.
	Icon string `xorm:"VARCHAR(60) NOT NULL DEFAULT ''"`
	// FolderID is the folder shown in the sidebar; 0 means the conversation is at the root.
	FolderID int64 `xorm:"INDEX NOT NULL DEFAULT 0"`
	// TitleSet records that a human or the model named the session, so a placeholder
	// title is never overwritten twice.
	TitleSet bool `xorm:"NOT NULL DEFAULT FALSE"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (Conversation) TableName() string {
	return "agent_conversation"
}

// Message roles.
const (
	MessageRoleUser      = "user"
	MessageRoleAssistant = "assistant"
	MessageRoleSystem    = "system"
)

// Message is one entry of a conversation transcript.
type Message struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	Role           string `xorm:"VARCHAR(16) NOT NULL"`
	Content        string `xorm:"TEXT NOT NULL"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
}

func (Message) TableName() string {
	return "agent_message"
}

// Run statuses. A run is one server-side execution of the agent for a user message.
const (
	RunStatusQueued    = "queued"
	RunStatusRunning   = "running"
	RunStatusSucceeded = "succeeded"
	RunStatusFailed    = "failed"
	RunStatusCanceled  = "canceled"
)

// IsRunActive reports whether the run is still going to produce output.
func IsRunActive(status string) bool {
	return status == RunStatusQueued || status == RunStatusRunning
}

// Run is one server-side agent execution. It is stored before the work starts so a
// client can disconnect, and the instance can recover after a restart.
type Run struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	Status         string `xorm:"VARCHAR(16) NOT NULL"`
	Model          string `xorm:"VARCHAR(160) NOT NULL DEFAULT ''"` // model that answered
	Error          string `xorm:"TEXT"`                             // failure reason, when failed

	CreatedUnix  timeutil.TimeStamp `xorm:"created NOT NULL"`
	StartedUnix  timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
	FinishedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (Run) TableName() string {
	return "agent_run"
}

func init() {
	db.RegisterModel(new(Conversation))
	db.RegisterModel(new(Message))
	db.RegisterModel(new(Run))
}

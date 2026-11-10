// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"

	user_model "gitea.dev/models/user"
)

// AskUserToolName is the tool that puts a question in front of a reader: the run waits in the
// thread it was asked in until someone answers it.
const AskUserToolName = "gitea_ask_user"

// askUserTool asks the reader what the run cannot work out on its own.
type askUserTool struct{}

func (askUserTool) Name() string { return AskUserToolName }

func (askUserTool) Description() string {
	return "Ask the reader a question when the work can not go on without their answer: which of two " +
		"directions to take, a value only they know, whether to go ahead with something unusual. The " +
		"question reaches them in the thread the run is working in, and the run waits for the answer; " +
		"ask once, with what the answer would change."
}

func (askUserTool) Scope() Scope { return ScopeUser }

// Writes reports whether the call changes the instance: a question changes nothing by itself.
func (askUserTool) Writes() bool { return false }

// DefaultMode is the reader's: a question that were answered by the model is not a question.
func (askUserTool) DefaultMode() string { return PermissionManual }

func (askUserTool) Args() []Arg {
	return []Arg{
		{Name: "question", Description: "What to ask, in the language the reader writes in"},
		{Name: "options", Optional: true, Description: `Answers to offer as choices, separated by "|", when a few will do`},
		{Name: "why", Optional: true, Description: "One line on what the answer changes, shown under the question"},
	}
}

// Run is for a run that did not wait, which is what autopilot does with a question: the model is
// told to decide for itself rather than to wait for a reader who was not asked.
func (askUserTool) Run(_ context.Context, _ *user_model.User, _ Session, _ string) (string, error) {
	return "No reader was asked: decide with what you have, and say what you assumed.", nil
}

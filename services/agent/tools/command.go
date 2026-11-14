// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"strings"

	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/services/agent/sandbox"
)

// runCommandTool runs one command of the session's repository in a container of this instance's
// own, so a run can build, test or lint what it is working on rather than guess whether it holds.
type runCommandTool struct{}

func (runCommandTool) Name() string { return "gitea_run_command" }

func (runCommandTool) Description() string {
	return "Run one shell command of this repository in a container, at the revision you are working " +
		"on, and read what it wrote. Use it to build, test or lint a change instead of guessing " +
		"whether it holds. The checkout is a copy: nothing you do here reaches the repository, and " +
		"the container has no network unless the instance allows one."
}

func (runCommandTool) Scope() Scope { return ScopeRepository }

// Writes says this tool changes something, and DefaultMode that it asks its reader first: a run
// offers it from the start, and every command is put to the reader it works for.
func (runCommandTool) Writes() bool { return true }

func (runCommandTool) DefaultMode() string { return PermissionManual }

// Offered keeps the tool out of a session on an instance that cannot run a command at all, so the
// model is not told about something that would only fail.
func (runCommandTool) Offered(ctx context.Context, session Session) (bool, error) {
	if session.RepoID == 0 {
		return false, nil
	}
	_, ok := sandbox.Available()
	return ok, nil
}

func (runCommandTool) Args() []Arg {
	return []Arg{
		repositoryArg("the command"),
		{Name: "command", Description: "Shell command to run, like `go test ./services/agent/...`"},
		{Name: "ref", Optional: true, Description: "Branch, tag or commit to work on; leave it out for the default branch"},
	}
}

func (runCommandTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := runCommandTool{}.run(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (runCommandTool) run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Command    string `json:"command"`
		Ref        string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	command := strings.TrimSpace(params.Command)
	if command == "" {
		return "The command argument is required.", nil
	}
	runtime, ok := sandbox.Available()
	if !ok {
		return "This instance does not run commands: an administrator has to set [agent] COMMANDS " +
			"and COMMAND_IMAGE, and have a container runtime. Say what you would have run instead.", nil
	}
	if len(command) > 4096 {
		return "That command is longer than this instance runs.", nil
	}

	repository, err := resolveRepo(ctx, doer, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	// the checkout of this revision is shared with the other commands of the run, so what one of
	// them built is there for the next, and a commit the run made is what the next one reads
	work, err := sandbox.Workspace(ctx, repository, params.Ref)
	if err != nil {
		return "", err
	}

	output, err := sandbox.Run(ctx, runtime, work.Dir, command)
	head := "Ran `" + command + "` in " + setting.Agent.CommandImage + " at " + work.Ref +
		" of " + repository.FullName() + ".\n"
	if err != nil {
		// what a command wrote before it failed is the answer the model needs; the failure itself
		// is said after it
		return clip(head + "\n" + output + "\n" + err.Error()), nil
	}
	if strings.TrimSpace(output) == "" {
		return head + "It wrote nothing and succeeded.", nil
	}
	return clip(head + "\n" + output), nil
}

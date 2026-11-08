// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"

	actions_model "gitea.dev/models/actions"
	db_list "gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/base"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
)

// listRunsTool reads the continuous integration of a repository: what ran, where, and how it
// ended. A branch that does not pass is the first thing a change has to answer for.
type listRunsTool struct{}

func (listRunsTool) Name() string { return "gitea_list_actions_runs" }

func (listRunsTool) Description() string {
	return "List the CI runs of a repository, newest first, or the jobs of one run when its number " +
		"is given. Use it to see whether the work of a branch passed, and which job of a run failed."
}

func (listRunsTool) Scope() Scope { return ScopeRepository }

func (listRunsTool) Args() []Arg {
	return []Arg{
		repositoryArg("the CI runs"),
		{Name: "ref", Optional: true, Description: `Branch to look at, like "main"; leave it out for every branch`},
		{
			Name: "status", Optional: true,
			Description: `Which runs to return: "success", "failure", "running", "waiting", "cancelled" or "all" (the default)`,
		},
		{Name: "run", Type: "integer", Optional: true, Description: "The number of one run, to list its jobs instead"},
	}
}

func (listRunsTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := listRunsTool{}.list(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (listRunsTool) list(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		Status     string `json:"status"`
		Run        int64  `json:"run"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, doer, session, params.Repository, unit_model.TypeActions)
	if err != nil {
		return "", err
	}
	if params.Run != 0 {
		return runJobs(ctx, repository, params.Run)
	}
	statuses, err := parseRunStatuses(params.Status)
	if err != nil {
		return "", err
	}
	options := actions_model.FindRunOptions{
		RepoID:      repository.ID,
		Status:      statuses,
		ListOptions: db_list.ListOptions{Page: 1, PageSize: toolResultLimit},
	}
	if ref := strings.TrimSpace(params.Ref); ref != "" {
		if !strings.HasPrefix(ref, "refs/") {
			ref = "refs/heads/" + ref
		}
		options.Ref = ref
	}
	runs, err := db_list.Find[actions_model.ActionRun](ctx, options)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "No CI runs of " + repository.FullName() + " match that.", nil
	}
	lines := make([]string, 0, len(runs))
	for _, run := range runs {
		lines = append(lines, runLine(run))
	}
	log.Trace("Agent tool gitea_list_actions_runs listed %d runs of %s for user %d", len(runs), repository.FullName(), doer.ID)
	return clip(strings.Join(lines, "\n")), nil
}

// runJobs lists the jobs of one run: a run says whether it passed, its jobs say what did not.
func runJobs(ctx context.Context, repository *repo_model.Repository, index int64) (string, error) {
	run := &actions_model.ActionRun{}
	has, err := db_list.GetEngine(ctx).Where("repo_id = ?", repository.ID).And("`index` = ?", index).Get(run)
	if err != nil {
		return "", err
	}
	if !has {
		return fmt.Sprintf("There is no CI run #%d in %s.", index, repository.FullName()), nil
	}
	jobs, err := db_list.Find[actions_model.ActionRunJob](ctx, actions_model.FindRunJobOptions{
		RunID: run.ID, RepoID: repository.ID, ListOptions: db_list.ListOptions{Page: 1, PageSize: toolResultLimit},
	})
	if err != nil {
		return "", err
	}
	lines := []string{runLine(run)}
	for _, job := range jobs {
		lines = append(lines, "- "+job.Name+" ("+job.Status.String()+")")
	}
	if len(jobs) == 0 {
		lines = append(lines, "- no job is recorded for it")
	}
	log.Trace("Agent tool gitea_list_actions_runs listed %d jobs of run %s#%d", len(jobs), repository.FullName(), index)
	return clip(strings.Join(lines, "\n")), nil
}

// runLine says what one run was, where it ran, and how it ended.
func runLine(run *actions_model.ActionRun) string {
	return fmt.Sprintf("#%d %s %s on %s at %s", run.Index, run.WorkflowID, run.Status.String(),
		shortRef(run.Ref), base.ShortSha(run.CommitSHA))
}

// shortRef names the branch a run was for, the way a reader says it.
func shortRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// parseRunStatuses reads the state a call asked about; nothing means every state.
func parseRunStatuses(raw string) ([]actions_model.Status, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "all":
		return nil, nil
	case "success":
		return []actions_model.Status{actions_model.StatusSuccess}, nil
	case "failure":
		return []actions_model.Status{actions_model.StatusFailure}, nil
	case "running":
		return []actions_model.Status{actions_model.StatusRunning}, nil
	case "cancelled":
		return []actions_model.Status{actions_model.StatusCancelled}, nil
	case "waiting":
		return []actions_model.Status{actions_model.StatusWaiting, actions_model.StatusBlocked, actions_model.StatusPending}, nil
	}
	return nil, unreadableRepo{`The status is "success", "failure", "running", "waiting", "cancelled" or "all".`}
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	pull_service "gitea.dev/services/pull"
	repo_service "gitea.dev/services/repository"
)

// mergePullRequestTool lands a change: it is the last step of the path a mention starts, and the
// one a repository's own rules and the reader's decision are meant to guard.
type mergePullRequestTool struct{}

func (mergePullRequestTool) Name() string { return "gitea_merge_pull_request" }

func (mergePullRequestTool) Description() string {
	return "Merge a pull request into the branch it was asked for, once its checks and reviews " +
		"allow it: land a change you have read and that does what was asked, and leave one you " +
		"could not check for a reader. Only the merge styles the repository allows are accepted, " +
		"and a branch protection rule that is not satisfied refuses the merge."
}

func (mergePullRequestTool) Scope() Scope { return ScopeRepository }

func (mergePullRequestTool) Writes() bool { return true }

func (mergePullRequestTool) Args() []Arg {
	return []Arg{
		repositoryArg("the change"),
		{Name: "index", Type: "integer", Description: "The number gitea_search_issues showed for it"},
		{Name: "style", Optional: true, Description: `"merge" (the default), "squash", "rebase" or "rebase-merge"`},
		{Name: "message", Optional: true, Description: "The merge commit message; the repository's own wording is used when this is left out"},
		{Name: "delete_branch", Optional: true, Type: "boolean", Description: "Delete the branch the change was on afterwards; the repository's own setting decides when this is left out"},
	}
}

func (mergePullRequestTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := mergePullRequestTool{}.merge(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (mergePullRequestTool) merge(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository   string `json:"repository"`
		Index        int64  `json:"index"`
		Style        string `json:"style"`
		Message      string `json:"message"`
		DeleteBranch *bool  `json:"delete_branch"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	if params.Index == 0 {
		return "The index argument is required.", nil
	}
	actor, err := actorOf(ctx, doer, session)
	if err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, actor, session, params.Repository, unit_model.TypePullRequests)
	if err != nil {
		return "", err
	}
	style := repo_model.MergeStyle(strings.TrimSpace(params.Style))
	if style == "" {
		style = repo_model.MergeStyleMerge
	}
	// the styles a repository offers are its own business: a session picks from them, and is told
	// which ones are on offer rather than being left with a refusal from git
	prUnit, err := repository.GetUnit(ctx, unit_model.TypePullRequests)
	if err != nil {
		return "", err
	}
	config := prUnit.PullRequestsConfig()
	if !config.IsMergeStyleAllowed(style) {
		return fmt.Sprintf("%s does not allow the %s merge style. It allows: %s.",
			repository.FullName(), style, allowedMergeStyles(config)), nil
	}
	pr, err := issues_model.GetPullRequestByIndex(ctx, repository.ID, params.Index)
	if err != nil {
		if issues_model.IsErrPullRequestNotExist(err) {
			return fmt.Sprintf("There is no pull request %s #%d.", repository.FullName(), params.Index), nil
		}
		return "", err
	}
	if err := pr.LoadHeadRepo(ctx); err != nil {
		return "", err
	}
	if err := pr.LoadIssue(ctx); err != nil {
		return "", err
	}
	pr.Issue.Repo = repository
	permission, err := access_model.GetDoerRepoPermission(ctx, repository, actor)
	if err != nil {
		return "", err
	}
	if err := pull_service.CheckPullMergeable(ctx, actor, &permission, pr, pull_service.MergeCheckTypeGeneral, style, false); err != nil {
		switch {
		case errors.Is(err, pull_service.ErrIsClosed):
			return fmt.Sprintf("Pull request %s #%d is closed, so there is nothing to merge.",
				repository.FullName(), pr.Index), nil
		case errors.Is(err, pull_service.ErrHasMerged):
			return fmt.Sprintf("Pull request %s #%d is already merged.", repository.FullName(), pr.Index), nil
		case errors.Is(err, pull_service.ErrNoPermissionToMerge):
			return fmt.Sprintf("You are not allowed to merge pull request %s #%d.", repository.FullName(), pr.Index), nil
		case errors.Is(err, pull_service.ErrIsWorkInProgress):
			return fmt.Sprintf("Pull request %s #%d is a work in progress: take the marker out of its title first.",
				repository.FullName(), pr.Index), nil
		case errors.Is(err, pull_service.ErrNotMergeableState), errors.Is(err, pull_service.ErrIsChecking):
			return fmt.Sprintf("Pull request %s #%d is not in a mergeable state: read it with gitea_read_pull_request, and read what conflicts with gitea_read_file.",
				repository.FullName(), pr.Index), nil
		case errors.Is(err, pull_service.ErrNotReadyToMerge):
			return fmt.Sprintf("The branch %s protects itself, and %s #%d is not ready for it yet: %s.",
				pr.BaseBranch, repository.FullName(), pr.Index, err.Error()), nil
		case errors.Is(err, pull_service.ErrDependenciesLeft):
			return fmt.Sprintf("Pull request %s #%d still has open dependencies.", repository.FullName(), pr.Index), nil
		}
		return "", err
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	message := strings.TrimSpace(params.Message)
	if message == "" {
		message, _, err = pull_service.GetDefaultMergeMessage(ctx, gitRepo, pr, style)
		if err != nil {
			return "", err
		}
	}
	if err := pull_service.Merge(pr.ID, actor, style, "", message, false); err != nil {
		switch {
		case pull_service.IsErrInvalidMergeStyle(err):
			return fmt.Sprintf("%s does not allow the %s merge style.", repository.FullName(), style), nil
		case errors.As(err, new(pull_service.ErrMergeConflicts)), errors.As(err, new(pull_service.ErrRebaseConflicts)):
			return fmt.Sprintf("Merging %s #%d conflicts with %s: read both sides with gitea_read_file, resolve the change on the branch, and ask again.",
				repository.FullName(), pr.Index, pr.BaseBranch), nil
		case errors.As(err, new(pull_service.ErrMergeUnrelatedHistories)):
			return fmt.Sprintf("The branches of %s #%d share no history, so git cannot merge them.", repository.FullName(), pr.Index), nil
		case pull_service.IsErrSHADoesNotMatch(err), git.IsErrPushOutOfDate(err):
			return fmt.Sprintf("The branch of %s #%d moved while it was being merged: read it again and ask again.",
				repository.FullName(), pr.Index), nil
		}
		return "", err
	}
	merged := ""
	if head, err := gitRepo.GetBranchCommitID(ctx, pr.BaseBranch); err == nil {
		merged = shortSha(head)
	}
	// the branch a change came from is spent once the change landed, when the repository says so
	deleted := ""
	if config, err := pull_service.ShouldDeleteBranchAfterMerge(ctx, params.DeleteBranch, repository, pr); err == nil && config {
		var branch string
		if err := repo_service.DeleteBranchAfterMerge(ctx, actor, pr.ID, &branch); err != nil {
			log.Debug("Agent tool gitea_merge_pull_request kept %s of %s: %v", pr.HeadBranch, repository.FullName(), err)
		} else {
			deleted = fmt.Sprintf(" The branch %s is gone with it.", branch)
		}
	}
	log.Trace("Agent tool gitea_merge_pull_request merged %s #%d for user %d", repository.FullName(), pr.Index, doer.ID)
	return fmt.Sprintf("Merged %s #%d (%s) into %s as %s.%s The change is on the branch it was asked for now.",
		repository.FullName(), pr.Index, style, pr.BaseBranch, merged, deleted), nil
}

// allowedMergeStyles names the styles a repository offers, in the words a call would use.
func allowedMergeStyles(config *repo_model.PullRequestsConfig) string {
	styles := make([]string, 0, 4)
	for _, style := range []repo_model.MergeStyle{
		repo_model.MergeStyleMerge, repo_model.MergeStyleRebase, repo_model.MergeStyleRebaseMerge, repo_model.MergeStyleSquash,
	} {
		if config.IsMergeStyleAllowed(style) {
			styles = append(styles, `"`+string(style)+`"`)
		}
	}
	if len(styles) == 0 {
		return "none"
	}
	return strings.Join(styles, ", ")
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"

	issues_model "gitea.dev/models/issues"
	access_model "gitea.dev/models/perm/access"
	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/services/gitdiff"
)

// readPullRequestTool reads what a pull request changes, which is what reviewing one is about:
// the files it touches, and the patch of any one of them.
type readPullRequestTool struct{}

func (readPullRequestTool) Name() string { return "gitea_read_pull_request" }

func (readPullRequestTool) Description() string {
	return "Read what a pull request changes: the files it touches with their added and removed " +
		"lines, or the patch of one of them when a path is given. gitea_read_issue reads the pull " +
		"request itself: its description and the branches it merges."
}

func (readPullRequestTool) Scope() Scope { return ScopeRepository }

func (readPullRequestTool) Args() []Arg {
	return []Arg{
		repositoryArg("the pull request"),
		{Name: "index", Type: "integer", Description: "The number gitea_search_issues showed for it"},
		{
			Name: "path", Optional: true,
			Description: "One changed file, to read its patch; leave it out to list every file the pull request changes",
		},
	}
}

func (readPullRequestTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := readPullRequestTool{}.read(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (readPullRequestTool) read(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Index      int64  `json:"index"`
		Path       string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	if params.Index == 0 {
		return "The index argument is required.", nil
	}
	repository, err := resolveRepo(ctx, doer, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	issue, err := issues_model.GetIssueByIndex(ctx, repository.ID, params.Index)
	if err != nil {
		return "There is no issue or pull request with that number in " + repository.FullName() + ".", nil
	}
	if !issue.IsPull {
		return fmt.Sprintf("That is issue #%d, not a pull request: it changes nothing. Read it with %s.",
			issue.Index, readIssueToolName), nil
	}
	if err := issue.LoadRepo(ctx); err != nil {
		return "", err
	}
	if err := issue.LoadPullRequest(ctx); err != nil {
		return "", err
	}
	perm, err := access_model.GetIndividualUserRepoPermission(ctx, issue.Repo, doer)
	if err != nil {
		return "", err
	}
	if !perm.CanReadIssuesOrPulls(true) {
		return "That pull request is not visible to you.", nil
	}

	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	head, err := gitRepo.GetRefCommitID(ctx, issue.PullRequest.GetGitHeadRefName())
	if err != nil {
		return "", err
	}
	// the merge base is where the two branches parted, so the diff is the work of the request
	// itself; a request without one reads against the branch it is meant for
	base := issue.PullRequest.MergeBase
	if base == "" {
		base = issue.PullRequest.BaseBranch
	}
	options := &gitdiff.DiffOptions{
		DiffCommonOptions: gitdiff.DiffCommonOptions{BeforeCommitID: base, AfterCommitID: head},
		MaxLines:          setting.Git.MaxGitDiffLines,
		MaxLineCharacters: setting.Git.MaxGitDiffLineCharacters,
		MaxFiles:          -1, // every file, so the model sees the whole change
	}
	path, ok := cleanPath(params.Path)
	if !ok {
		return "That path is one a repository cannot hold.", nil
	}
	if path == "" {
		diff, err := gitdiff.GetDiffForAPI(ctx, gitRepo, options)
		if err != nil {
			return "", err
		}
		log.Trace("Agent tool gitea_read_pull_request listed %d files of %s#%d for user %d",
			len(diff.Files), repository.FullName(), issue.Index, doer.ID)
		return clip(pullRequestFiles(diff, issue)), nil
	}
	diff, err := gitdiff.GetDiffForAPI(ctx, gitRepo, options, path)
	if err != nil {
		return "", err
	}
	if len(diff.Files) == 0 {
		return "That pull request does not change `" + path + "`.", nil
	}
	log.Trace("Agent tool gitea_read_pull_request read the patch of %s of %s#%d for user %d",
		path, repository.FullName(), issue.Index, doer.ID)
	return clip(pullRequestPatch(diff.Files[0])), nil
}

// readIssueToolName is named where another tool answers with what to use instead.
const readIssueToolName = "gitea_read_issue"

// pullRequestFiles says which files a pull request changes, and by how much.
func pullRequestFiles(diff *gitdiff.Diff, issue *issues_model.Issue) string {
	if len(diff.Files) == 0 {
		return fmt.Sprintf("Pull request %s #%d changes no file.", issue.Repo.FullName(), issue.Index)
	}
	added, removed := 0, 0
	lines := make([]string, 0, len(diff.Files)+2)
	lines = append(lines, fmt.Sprintf("%s #%d %s", issue.Repo.FullName(), issue.Index, issue.Title))
	for _, file := range diff.Files {
		added += file.Addition
		removed += file.Deletion
		lines = append(lines, "- "+fileLine(file))
	}
	lines = append(lines, fmt.Sprintf("%d files changed, +%d −%d", len(diff.Files), added, removed))
	// the changes are not on the branch it is meant for yet, so reading one of the files needs
	// the branch they are written on
	lines = append(lines, "The changed files are on "+issue.PullRequest.HeadBranch+": read one with gitea_read_file and that ref.")
	if diff.IsIncomplete {
		lines = append(lines, "(the change is too large to be read in full)")
	}
	return strings.Join(lines, "\n")
}

// fileLine says what one file of a change is, and what happened to it.
func fileLine(file *gitdiff.DiffFile) string {
	name := "`" + file.Name + "`"
	switch {
	case file.IsRenamed:
		name = "`" + file.OldName + "` → `" + file.Name + "`"
	case file.IsCreated:
		name += " (added)"
	case file.IsDeleted:
		name += " (deleted)"
	}
	switch {
	case file.IsSubmodule:
		return name + " (submodule)"
	case file.IsBin:
		return name + " (binary)"
	}
	return fmt.Sprintf("%s (+%d −%d)", name, file.Addition, file.Deletion)
}

// pullRequestPatch renders one file of a change the way a patch reads, hunk by hunk.
func pullRequestPatch(file *gitdiff.DiffFile) string {
	builder := strings.Builder{}
	builder.WriteString(fileLine(file))
	for _, section := range file.Sections {
		builder.WriteString("\n")
		for _, line := range section.Lines {
			builder.WriteString(line.Content)
			builder.WriteString("\n")
		}
	}
	if file.IsIncomplete {
		builder.WriteString("(the file is too large to be read in full)\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

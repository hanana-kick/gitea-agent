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
	pull_service "gitea.dev/services/pull"
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

// newPullRequestTool opens a pull request, which is how a change an agent committed reaches the
// branch a repository is meant for: as a change a reader can look at first.
type newPullRequestTool struct{}

func (newPullRequestTool) Name() string { return "gitea_create_pull_request" }

func (newPullRequestTool) Description() string {
	return "Open a pull request from a branch to another, so a change reaches the branch it is " +
		"meant for only once a reader has seen it. Use it after gitea_commit_files or " +
		"gitea_patch_file wrote a change to a branch; gitea_read_pull_request reads back what " +
		"such a change does."
}

func (newPullRequestTool) Scope() Scope { return ScopeRepository }

func (newPullRequestTool) Writes() bool { return true }

func (newPullRequestTool) Args() []Arg {
	return []Arg{
		repositoryArg("the change"),
		{Name: "head", Description: "The branch the change is on"},
		{Name: "base", Optional: true, Description: "The branch it should reach; leave it out for the default branch"},
		{Name: "title", Description: "The title of the pull request"},
		{Name: "body", Optional: true, Description: "The description: what the change does and why"},
	}
}

func (newPullRequestTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := newPullRequestTool{}.open(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (newPullRequestTool) open(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Head       string `json:"head"`
		Base       string `json:"base"`
		Title      string `json:"title"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	head := strings.TrimSpace(params.Head)
	title := strings.TrimSpace(params.Title)
	if head == "" || title == "" {
		return "The head and title arguments are required.", nil
	}
	actor, err := actorOf(ctx, doer, session)
	if err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, actor, session, params.Repository, unit_model.TypePullRequests)
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(params.Base)
	if base == "" {
		base = repository.DefaultBranch
	}
	if head == base {
		return fmt.Sprintf("A pull request from %s to itself would change nothing.", head), nil
	}
	if head == repository.DefaultBranch {
		return fmt.Sprintf("%s is the branch of %s that changes land in: open the pull request from the branch the change is on, to %s.",
			head, repository.FullName(), base), nil
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	if !git.IsBranchExist(ctx, gitRepo, base) {
		return fmt.Sprintf("The branch %s is not in %s.", base, repository.FullName()), nil
	}
	if !git.IsBranchExist(ctx, gitRepo, head) {
		return fmt.Sprintf("The branch %s is not in %s: commit the change to it first.", head, repository.FullName()), nil
	}
	baseID, err := gitRepo.GetBranchCommitID(ctx, base)
	if err != nil {
		return "", err
	}
	headID, err := gitRepo.GetBranchCommitID(ctx, head)
	if err != nil {
		return "", err
	}
	if baseID == headID {
		return fmt.Sprintf("The branch %s has no change of its own yet, so a pull request from it would be empty.", head), nil
	}
	mergeBase, err := git.MergeBase(ctx, gitRepo, baseID, headID)
	if err != nil {
		return fmt.Sprintf("The branches %s and %s have no common history, so no pull request can be made between them.", base, head), nil
	}
	// an open pull request for the same pair is what a reader would be asked to look at twice
	existing, err := issues_model.GetUnmergedPullRequest(ctx, repository.ID, repository.ID, head, base, issues_model.PullRequestFlowGithub)
	if err != nil && !issues_model.IsErrPullRequestNotExist(err) {
		return "", err
	}
	if existing != nil {
		return fmt.Sprintf("The pull request %s #%d already asks for %s to reach %s: read it with gitea_read_pull_request.",
			repository.FullName(), existing.Index, head, base), nil
	}
	issue := &issues_model.Issue{
		RepoID:   repository.ID,
		Repo:     repository,
		Title:    title,
		PosterID: actor.ID,
		Poster:   actor,
		IsPull:   true,
		Content:  strings.TrimSpace(params.Body),
	}
	pullRequest := &issues_model.PullRequest{
		HeadRepoID: repository.ID,
		BaseRepoID: repository.ID,
		HeadBranch: head,
		BaseBranch: base,
		HeadRepo:   repository,
		BaseRepo:   repository,
		MergeBase:  mergeBase,
		Type:       issues_model.PullRequestGitea,
	}
	err = pull_service.NewPullRequest(ctx, &pull_service.NewPullRequestOptions{
		Repo:        repository,
		Issue:       issue,
		PullRequest: pullRequest,
	})
	if err != nil {
		return "", err
	}
	log.Trace("Agent tool gitea_create_pull_request opened %s #%d for user %d", repository.FullName(), issue.Index, doer.ID)
	return fmt.Sprintf("Opened pull request #%d in %s: %s into %s. Read what it changes with gitea_read_pull_request, and review it with gitea_review_pull_request.",
		issue.Index, repository.FullName(), head, base), nil
}

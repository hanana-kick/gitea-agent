// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"

	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/base"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/services/gitdiff"
	files_service "gitea.dev/services/repository/files"
)

// shortSha is the head of a commit id, the way a reader says it.
func shortSha(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// listCommitsTool walks the history of a revision, newest first.
type listCommitsTool struct{}

func (listCommitsTool) Name() string { return "gitea_list_commits" }

func (listCommitsTool) Description() string {
	return "List the newest commits of a repository, optionally only those touching one path. " +
		"Use it to see what has been changing and by whom; gitea_read_commit shows one commit in full."
}

func (listCommitsTool) Scope() Scope { return ScopeRepository }

func (listCommitsTool) Args() []Arg {
	return []Arg{
		repositoryArg("the commit history"),
		{Name: "ref", Optional: true, Description: "Branch, tag or commit to walk; leave it out for the default branch"},
		{Name: "path", Optional: true, Description: "Only commits touching this path"},
	}
}

func (listCommitsTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := listCommitsTool{}.list(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (listCommitsTool) list(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		Path       string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	found, err := openSessionRepo(ctx, doer, session, params.Repository, params.Ref)
	if err != nil {
		return "", err
	}
	defer found.gitRepo.Close()

	var commits []*git.Commit
	if params.Path == "" {
		// the log of the whole revision, newest first, the fixture repos having few commits
		commits, err = found.gitRepo.ShowPrettyFormatLogToList(ctx, found.ref)
		if err == nil && len(commits) > toolResultLimit {
			commits = commits[:toolResultLimit]
		}
	} else {
		// walking the log of one path needs the log walk this tree uses for the file views
		tip, tipErr := found.gitRepo.GetCommitByPath(ctx, params.Path)
		if tipErr != nil {
			return "", tipErr
		}
		commits, _, err = found.gitRepo.CommitsByFileAndRange(ctx, git.CommitsByFileAndRangeOptions{
			Revision: tip.ID.String(), File: params.Path, Page: 1,
		})
	}
	if err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "There are no commits of " + displayPath(params.Path) + " in " + found.ref + ".", nil
	}
	lines := make([]string, 0, len(commits))
	for _, commit := range commits {
		lines = append(lines, fmt.Sprintf("%s %s: %s", base.ShortSha(commit.ID.String()), commit.Author.Name, commit.CommitMessage.MessageTitle()))
	}
	log.Trace("Agent tool gitea_list_commits listed %d commits of %s for user %d", len(commits), found.repository.FullName(), doer.ID)
	return clip(strings.Join(lines, "\n")), nil
}

// CommitFilesTool is the exported view of the write tool, for the tests that run the real
// engine against it.
type CommitFilesTool = commitFilesTool

// ReadFileTool is the exported view of the file reader.
type ReadFileTool = readFileTool

// readCommitTool reads one commit: the message it carries and the change it made.
type readCommitTool struct{}

func (readCommitTool) Name() string { return "gitea_read_commit" }

func (readCommitTool) Description() string {
	return "Read one commit: the full message, the author, and the files it changed with their added " +
		"and removed lines. Use the short ids gitea_list_commits shows."
}

func (readCommitTool) Scope() Scope { return ScopeRepository }

func (readCommitTool) Args() []Arg {
	return []Arg{
		repositoryArg("the commit"),
		{Name: "sha", Description: "The commit id gitea_list_commits showed"},
	}
}

func (readCommitTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := readCommitTool{}.read(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (readCommitTool) read(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		SHA        string `json:"sha"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	found, err := openSessionRepo(ctx, doer, session, params.Repository, params.SHA)
	if err != nil {
		return "", err
	}
	defer found.gitRepo.Close()
	commit := found.commit

	out := []string{fmt.Sprintf("%s by %s (%s)", base.ShortSha(commit.ID.String()), commit.Author.Name, found.ref)}
	if message := strings.TrimSpace(commit.CommitMessage.MessageUTF8()); message != "" {
		out = append(out, message)
	}
	options := &gitdiff.DiffOptions{MaxLines: setting.Git.MaxGitDiffLines, MaxLineCharacters: setting.Git.MaxGitDiffLineCharacters, MaxFiles: -1}
	if commit.ParentCount() > 0 {
		parent, err := commit.Parent(ctx, found.gitRepo, 0)
		if err != nil {
			return "", err
		}
		options.DiffCommonOptions = gitdiff.DiffCommonOptions{BeforeCommitID: parent.ID.String(), AfterCommitID: commit.ID.String()}
		diff, err := gitdiff.GetDiffForAPI(ctx, found.gitRepo, options)
		if err != nil {
			return "", err
		}
		for _, file := range diff.Files {
			out = append(out, "- "+fileLine(file))
		}
	}
	log.Trace("Agent tool gitea_read_commit read %s of %s for user %d", commit.ID.String(), found.repository.FullName(), doer.ID)
	return clip(strings.Join(out, "\n")), nil
}

// listBranchesTool names the branches a repository has, and which one is the default.
type listBranchesTool struct{}

func (listBranchesTool) Name() string { return "gitea_list_branches" }

func (listBranchesTool) Description() string {
	return "List the branches of a repository, with the default one named. Use it before you commit " +
		"to a branch or open a pull request against one."
}

func (listBranchesTool) Scope() Scope { return ScopeRepository }

func (listBranchesTool) Args() []Arg {
	return []Arg{repositoryArg("the branches")}
}

func (listBranchesTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := listBranchesTool{}.list(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (listBranchesTool) list(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, doer, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	names, _, err := gitRepo.GetBranchNames(ctx, 0, toolResultLimit)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "The repository " + repository.FullName() + " has no branches.", nil
	}
	lines := make([]string, 0, len(names))
	for _, name := range names {
		if name == repository.DefaultBranch {
			name += " (the default branch)"
		}
		lines = append(lines, name)
	}
	log.Trace("Agent tool gitea_list_branches listed %d branches of %s for user %d", len(names), repository.FullName(), doer.ID)
	return clip(strings.Join(lines, "\n")), nil
}

// searchCodeTool finds where a text appears in a repository's code.
type searchCodeTool struct{}

func (searchCodeTool) Name() string { return "gitea_search_code" }

func (searchCodeTool) Description() string {
	return "Search a repository's code for a keyword, returning path, line and text. Use it to find " +
		"where something is written when you do not know the file."
}

func (searchCodeTool) Scope() Scope { return ScopeRepository }

func (searchCodeTool) Args() []Arg {
	return []Arg{
		repositoryArg("the code"),
		{Name: "keyword", Description: "Text to look for, verbatim"},
		{Name: "path", Optional: true, Description: "Directory or file to search within, like `services/agent/`"},
	}
}

func (searchCodeTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := searchCodeTool{}.search(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (searchCodeTool) search(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Keyword    string `json:"keyword"`
		Path       string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	params.Keyword = strings.TrimSpace(params.Keyword)
	if params.Keyword == "" {
		return "The keyword argument is required.", nil
	}
	found, err := openSessionRepo(ctx, doer, session, params.Repository, "")
	if err != nil {
		return "", err
	}
	defer found.gitRepo.Close()

	results, err := git.GrepSearch(ctx, found.gitRepo, params.Keyword, git.GrepOptions{
		RefName:           found.ref,
		MaxResultLimit:    toolResultLimit,
		ContextLineNumber: 0,
		PathspecList:      pathspecs(params.Path),
	})
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return fmt.Sprintf("Nothing in %s at %s contains %s.", found.repository.FullName(), found.ref, params.Keyword), nil
	}
	log.Trace("Agent tool gitea_search_code found %d files in %s for user %d", len(results), found.repository.FullName(), doer.ID)
	lines := make([]string, 0, len(results))
	for _, result := range results {
		for i, line := range result.LineCodes {
			lines = append(lines, fmt.Sprintf("%s:%d: %s", result.Filename, result.LineNumbers[i], line))
		}
	}
	return clip(strings.Join(lines, "\n")), nil
}

// pathspecs narrows a search to what was asked for; a directory is searched recursively.
func pathspecs(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return []string{strings.TrimSuffix(path, "/")}
}

// commitFilesTool changes files and commits them, on a branch that is not the default one.
type commitFilesTool struct{}

func (commitFilesTool) Name() string { return "gitea_commit_files" }

func (commitFilesTool) Description() string {
	return "Create, update or delete files in a repository and commit the change, on a branch. The " +
		"branch is created from `base` if it does not exist; the default branch cannot be committed " +
		"to. A pull request from that branch is how the change reaches it."
}

func (commitFilesTool) Scope() Scope { return ScopeRepository }

func (commitFilesTool) Writes() bool { return true }

func (commitFilesTool) Args() []Arg {
	return []Arg{
		repositoryArg("the files"),
		{Name: "branch", Description: "The branch to commit to; created from `base` when it does not exist"},
		{Name: "base", Optional: true, Description: "The branch to start the new one from; default the default branch"},
		{Name: "message", Description: "The commit message"},
		{Name: "files", Description: "The files to write or delete, as objects with `path` and `content`, or `path` and `delete` set true"},
	}
}

func (t commitFilesTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := t.commit(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (commitFilesTool) commit(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Base       string `json:"base"`
		Message    string `json:"message"`
		Files      []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Delete  bool   `json:"delete"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		// a model may send the files as the JSON text of the array rather than the array
		var loose struct {
			Repository string `json:"repository"`
			Branch     string `json:"branch"`
			Base       string `json:"base"`
			Message    string `json:"message"`
			Files      any    `json:"files"`
		}
		if json.Unmarshal([]byte(args), &loose) != nil {
			return "", err
		}
		params.Repository, params.Branch = loose.Repository, loose.Branch
		params.Base, params.Message = loose.Base, loose.Message
		if files, ok := loose.Files.(string); ok {
			if json.Unmarshal([]byte(files), &params.Files) != nil {
				return "", err
			}
		}
	}
	params.Branch = strings.TrimSpace(params.Branch)
	if params.Branch == "" || strings.TrimSpace(params.Message) == "" {
		return "The branch and message arguments are required.", nil
	}
	actor, err := actorOf(ctx, doer, session)
	if err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, actor, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	// a change an agent made never lands on the branch a repository is meant for: it goes to a
	// branch of its own, and reaches the default one as a pull request a reader can look at
	if params.Branch == repository.DefaultBranch {
		return fmt.Sprintf("The branch %s is the default branch of %s: commit to a branch of its own and open a pull request.",
			params.Branch, repository.FullName()), nil
	}
	if len(params.Files) == 0 {
		return "The files argument is required.", nil
	}

	files := make([]*files_service.ChangeRepoFile, 0, len(params.Files))
	for _, file := range params.Files {
		treePath, ok := cleanPath(file.Path)
		if !ok || treePath == "" {
			return fmt.Sprintf("%q is not a path a repository can hold.", file.Path), nil
		}
		// "upload" covers a file that is not there yet and one that is: the operation the web
		// editor would choose is decided by a path the model may not know
		operation := "upload"
		if file.Delete {
			operation = "delete"
		}
		entry := &files_service.ChangeRepoFile{Operation: operation, TreePath: treePath}
		if !file.Delete {
			entry.ContentReader = strings.NewReader(file.Content)
		}
		files = append(files, entry)
	}
	base := strings.TrimSpace(params.Base)
	if base == "" {
		base = repository.DefaultBranch
	}
	response, err := files_service.ChangeRepoFiles(ctx, repository, actor, &files_service.ChangeRepoFilesOptions{
		OldBranch: base,
		NewBranch: params.Branch,
		Message:   strings.TrimSpace(params.Message),
		Files:     files,
		Author:    &files_service.IdentityOptions{GitUserName: doer.Name, GitUserEmail: doer.Email},
		Committer: &files_service.IdentityOptions{GitUserName: doer.Name, GitUserEmail: doer.Email},
	})
	if err != nil {
		return "", err
	}
	commit := ""
	if response != nil && response.Commit != nil {
		commit = shortSha(response.Commit.SHA)
	}
	log.Trace("Agent tool gitea_commit_files wrote %d files of %s on %s for user %d", len(files), repository.FullName(), params.Branch, doer.ID)
	return fmt.Sprintf("Committed %d file(s) to %s in %s (%s). Open a pull request from it to reach %s.",
		len(files), params.Branch, repository.FullName(), commit, repository.DefaultBranch), nil
}

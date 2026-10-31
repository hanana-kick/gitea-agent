// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"fmt"
	"strings"

	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/reqctx"
	repo_service "gitea.dev/services/repository"
	files_service "gitea.dev/services/repository/files"
)

// createBranchTool makes a branch, so the changes of a review or a fix have somewhere to land
// that is not the default branch.
type createBranchTool struct{}

func (createBranchTool) Name() string { return "gitea_create_branch" }

func (createBranchTool) Description() string {
	return "Create a branch from another branch. Use it before committing files when the branch you " +
		"want to commit to does not exist yet; gitea_commit_files can create one itself."
}

func (createBranchTool) Scope() Scope { return ScopeRepository }

func (createBranchTool) Writes() bool { return true }

func (createBranchTool) Args() []Arg {
	return []Arg{
		repositoryArg("the branch"),
		{Name: "branch", Description: "The name of the branch to create"},
		{Name: "from", Optional: true, Description: "The branch to start from; leave it out for the default branch"},
	}
}

func (createBranchTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := createBranchTool{}.create(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (createBranchTool) create(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		From       string `json:"from"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	params.Branch = strings.TrimSpace(params.Branch)
	if params.Branch == "" {
		return "The branch argument is required.", nil
	}
	actor, err := actorOf(ctx, doer, session)
	if err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, actor, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	// the branch a repository is meant for is not for an agent to move: changes land on
	// branches of their own and arrive as pull requests
	if params.Branch == repository.DefaultBranch {
		return fmt.Sprintf("The branch %s is the default branch of %s: choose another name for yours.",
			params.Branch, repository.FullName()), nil
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	from := strings.TrimSpace(params.From)
	if from == "" {
		from = repository.DefaultBranch
	}
	if err := repo_service.CreateNewBranch(ctx, actor, repository, gitRepo, from, params.Branch); err != nil {
		return "", err
	}
	log.Trace("Agent tool gitea_create_branch created %s of %s for user %d", params.Branch, repository.FullName(), doer.ID)
	return fmt.Sprintf("Branch %s created from %s in %s.", params.Branch, from, repository.FullName()), nil
}

// CreateBranchTool is the exported view of the branch creator.
type CreateBranchTool = createBranchTool

// deleteBranchTool takes a branch away, which a session may do for the branches it made.
type deleteBranchTool struct{}

func (deleteBranchTool) Name() string { return "gitea_delete_branch" }

func (deleteBranchTool) Description() string {
	return "Delete a branch of a repository. The default branch cannot be deleted, nor one that an " +
		"open pull request is made from."
}

func (deleteBranchTool) Scope() Scope { return ScopeRepository }

func (deleteBranchTool) Writes() bool { return true }

func (deleteBranchTool) Args() []Arg {
	return []Arg{
		repositoryArg("the branch"),
		{Name: "branch", Description: "The name of the branch to delete"},
	}
}

func (deleteBranchTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := deleteBranchTool{}.del(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (deleteBranchTool) del(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	params.Branch = strings.TrimSpace(params.Branch)
	if params.Branch == "" {
		return "The branch argument is required.", nil
	}
	actor, err := actorOf(ctx, doer, session)
	if err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, actor, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	if params.Branch == repository.DefaultBranch {
		return fmt.Sprintf("The branch %s is the default branch of %s: it cannot be deleted.", params.Branch, repository.FullName()), nil
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return "", err
	}
	defer gitRepo.Close()
	if err := repo_service.DeleteBranch(ctx, actor, repository, gitRepo, params.Branch); err != nil {
		return "", err
	}
	log.Trace("Agent tool gitea_delete_branch deleted %s of %s for user %d", params.Branch, repository.FullName(), doer.ID)
	return fmt.Sprintf("Branch %s deleted from %s.", params.Branch, repository.FullName()), nil
}

// moveFileTool renames a file, keeping its history, which a delete-and-create would not.
type moveFileTool struct{}

func (moveFileTool) Name() string { return "gitea_move_file" }

func (moveFileTool) Description() string {
	return "Rename or move a file within a repository, keeping its history. Use it for reorganising " +
		"a tree; for changing what a file says, gitea_commit_files is the tool."
}

func (moveFileTool) Scope() Scope { return ScopeRepository }

func (moveFileTool) Writes() bool { return true }

func (moveFileTool) Args() []Arg {
	return []Arg{
		repositoryArg("the file"),
		{Name: "branch", Description: "The branch to commit the move to; created from `base` when it does not exist"},
		{Name: "from", Description: "The path of the file today"},
		{Name: "to", Description: "The path the file should have"},
		{Name: "message", Description: "The commit message"},
	}
}

func (moveFileTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := moveFileTool{}.move(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (moveFileTool) move(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		From       string `json:"from"`
		To         string `json:"to"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	params.Branch = strings.TrimSpace(params.Branch)
	from, ok := cleanPath(params.From)
	to, toOK := cleanPath(params.To)
	if params.Branch == "" || !ok || from == "" || !toOK || to == "" || strings.TrimSpace(params.Message) == "" {
		return "The branch, from, to and message arguments are required.", nil
	}
	actor, err := actorOf(ctx, doer, session)
	if err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, actor, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	if params.Branch == repository.DefaultBranch {
		return fmt.Sprintf("The branch %s is the default branch of %s: commit to a branch of its own and open a pull request.",
			params.Branch, repository.FullName()), nil
	}
	_, err = files_service.ChangeRepoFiles(ctx, repository, actor, &files_service.ChangeRepoFilesOptions{
		OldBranch: repository.DefaultBranch,
		NewBranch: params.Branch,
		Message:   strings.TrimSpace(params.Message),
		Files: []*files_service.ChangeRepoFile{{
			Operation:    "rename",
			FromTreePath: from,
			TreePath:     to,
		}},
	})
	if err != nil {
		return "", err
	}
	log.Trace("Agent tool gitea_move_file moved %s to %s of %s for user %d", from, to, repository.FullName(), doer.ID)
	return fmt.Sprintf("Moved %s to %s on %s in %s.", from, to, params.Branch, repository.FullName()), nil
}

// patchFileTool applies a unified diff to a file, which is how a code change reads best.
type patchFileTool struct{}

func (patchFileTool) Name() string { return "gitea_patch_file" }

func (patchFileTool) Description() string {
	return "Apply a unified diff to one file of a repository and commit the result on a branch. Use " +
		"it to make a code change the reader asked for; gitea_commit_files writes whole files instead."
}

func (patchFileTool) Scope() Scope { return ScopeRepository }

func (patchFileTool) Writes() bool { return true }

func (patchFileTool) Args() []Arg {
	return []Arg{
		repositoryArg("the file"),
		{Name: "branch", Description: "The branch to commit to; created from `base` when it does not exist"},
		{Name: "path", Description: "The path of the file the diff is about"},
		{Name: "diff", Description: "The unified diff (`git diff` shape) to apply"},
		{Name: "message", Description: "The commit message"},
	}
}

func (patchFileTool) Run(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	output, err := patchFileTool{}.patch(ctx, doer, session, args)
	if message, told := toldToModel(err); told {
		return message, nil
	}
	return output, err
}

func (patchFileTool) patch(ctx context.Context, doer *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Path       string `json:"path"`
		Diff       string `json:"diff"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	params.Branch = strings.TrimSpace(params.Branch)
	diff := strings.TrimSpace(params.Diff)
	if params.Branch == "" || diff == "" || strings.TrimSpace(params.Message) == "" {
		return "The branch, diff and message arguments are required.", nil
	}
	actor, err := actorOf(ctx, doer, session)
	if err != nil {
		return "", err
	}
	repository, err := resolveRepo(ctx, actor, session, params.Repository, unit_model.TypeCode)
	if err != nil {
		return "", err
	}
	if params.Branch == repository.DefaultBranch {
		return fmt.Sprintf("The branch %s is the default branch of %s: commit to a branch of its own and open a pull request.",
			params.Branch, repository.FullName()), nil
	}
	if !strings.HasPrefix(diff, "diff ") {
		// a patch without its header is applied against the named file
		diff = fmt.Sprintf("diff --git a/%s b/%s\n%s", params.Path, params.Path, diff)
	}
	response, err := files_service.ApplyDiffPatch(reqctx.FromContext(ctx), repository, actor, &files_service.ApplyDiffPatchOptions{
		OldBranch: repository.DefaultBranch,
		NewBranch: params.Branch,
		Message:   strings.TrimSpace(params.Message),
		Content:   diff,
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
	log.Trace("Agent tool gitea_patch_file patched %s of %s for user %d", params.Path, repository.FullName(), doer.ID)
	return fmt.Sprintf("Patch applied to %s on %s in %s (%s).", params.Path, params.Branch, repository.FullName(), commit), nil
}

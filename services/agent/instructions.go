// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"strings"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/git"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/typesniffer"
)

// instructionLimit is how much of one instruction file reaches the prompt: a repository may
// write a book, and the run still has to have room left to work.
const instructionLimit = 16 * 1024

// repositoryInstructions reads what a repository says about working in it — its AGENTS.md, and
// whatever else the instance names — so a run follows the house rules rather than guessing them.
// The text is the repository's own, so it is quoted as such: a run reads it as guidance, and a
// reader's request still comes first.
func repositoryInstructions(ctx context.Context, conversation *agent_model.Conversation) string {
	if conversation == nil || conversation.Kind != agent_model.ConversationKindRepository || conversation.RepoID == 0 {
		return ""
	}
	files := setting.Agent.Instructions
	if len(files) == 0 {
		return ""
	}
	repository, err := repo_model.GetRepositoryByID(ctx, conversation.RepoID)
	if err != nil || repository == nil {
		return ""
	}
	gitRepo, err := git.OpenRepository(ctx, repository)
	if err != nil {
		return ""
	}
	defer gitRepo.Close()
	commit, err := gitRepo.GetCommit(ctx, repository.DefaultBranch)
	if err != nil {
		return ""
	}

	read := make([]string, 0, len(files))
	for _, name := range files {
		if content := instructionAt(ctx, gitRepo, commit, name); content != "" {
			read = append(read, "### "+name+"\n"+content)
		}
	}
	if len(read) == 0 {
		return ""
	}
	return "The repository " + repository.FullName() + " says how to work in it. Follow it where the " +
		"reader's request leaves a choice:\n\n" + strings.Join(read, "\n\n")
}

// instructionAt reads one instruction file of the revision; a file that is not there, is not
// text, or says nothing is not a failure — most repositories say nothing.
func instructionAt(ctx context.Context, gitRepo *git.Repository, commit *git.Commit, path string) string {
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, path)
	if err != nil || entry.IsDir() {
		return ""
	}
	blob := entry.Blob(gitRepo)
	if blob.Size(ctx) <= 0 {
		return ""
	}
	content, err := blob.GetBlobBytes(ctx, instructionLimit)
	if err != nil || !typesniffer.DetectContentType(content).IsText() {
		return ""
	}
	text := strings.TrimSpace(string(content))
	if text == "" {
		return ""
	}
	return "```\n" + text + "\n```"
}

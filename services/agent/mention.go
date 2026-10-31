// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	agent_model "gitea.dev/models/agent"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	notify_service "gitea.dev/services/notify"
)

// AgentUser is the account the agent writes as on the surfaces it works on; nil when the
// instance has not named one, and then the agent answers nothing on its own.
func UserOf(ctx context.Context) *user_model.User {
	if setting.Agent.Username == "" {
		return nil
	}
	user, err := user_model.GetUserByName(ctx, setting.Agent.Username)
	if err != nil {
		return nil
	}
	return user
}

// answerMention starts a run for a comment that named the agent. The run reads what the
// mentioning user may read, and the answer is meant for the thread it came from.
func answerMention(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue, comment *issues_model.Comment, mentions []*user_model.User) {
	agent := UserOf(ctx)
	if agent == nil || doer == nil || repo == nil || issue == nil || comment == nil {
		return
	}
	// an agent does not answer itself: the comments it writes carry its own name, and would
	// otherwise name it again forever
	if doer.ID == agent.ID {
		return
	}
	if !slices.ContainsFunc(mentions, func(mentioned *user_model.User) bool { return mentioned.ID == agent.ID }) {
		return
	}
	if err := issue.LoadRepo(ctx); err != nil {
		log.Error("Agent mention: loading the repository of %s#%d failed: %v", repo.FullName(), issue.Index, err)
		return
	}
	session, err := repositorySessionOf(ctx, agent, repo)
	if err != nil {
		log.Error("Agent mention: the session of %s failed: %v", repo.FullName(), err)
		return
	}
	if _, _, err := SubmitUserMessage(ctx, session, doer.ID, mentionRequest(repo, issue, doer, comment)); err != nil {
		log.Error("Agent mention: starting a run for %s#%d failed: %v", repo.FullName(), issue.Index, err)
	}
}

// mentionRequest is what the run is told: where the mention came from, and where the answer
// belongs. It is written for the model, so it stays in English.
func mentionRequest(repo *repo_model.Repository, issue *issues_model.Issue, doer *user_model.User, comment *issues_model.Comment) string {
	builder := strings.Builder{}
	fmt.Fprintf(&builder, "[mention] @%s was named by @%s in a comment on %s #%d, %q.\n",
		setting.Agent.Username, doer.Name, repo.FullName(), issue.Index, issue.Title)
	fmt.Fprintf(&builder, "Answer in that thread with gitea_comment_issue (repository %s, index %d) once you have something to say; "+
		"gitea_read_issue and gitea_read_pull_request read what it is about.\n", repo.FullName(), issue.Index)
	builder.WriteString(strings.TrimSpace(comment.Content))
	return builder.String()
}

// repositorySessionOf is the session the agent works in for a repository: one per repository,
// kept so that what it learned about it is not forgotten between mentions.
func repositorySessionOf(ctx context.Context, owner *user_model.User, repo *repo_model.Repository) (*agent_model.Conversation, error) {
	sessions, err := findConversations(ctx, agent_model.ConversationKindRepository, owner.ID)
	if err != nil {
		return nil, err
	}
	for _, session := range sessions {
		if session.RepoID == repo.ID {
			return session, nil
		}
	}
	return CreateConversation(ctx, agent_model.ConversationKindRepository, owner.ID, repo.ID, repo.FullName())
}

// InitMentions makes the agent answer the comments that name it. It is called once, at start
// up, after the engine has its workers.
func InitMentions() {
	if setting.Agent.Username == "" {
		return
	}
	notify_service.CommentListener = answerMention
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	issues_service "gitea.dev/services/issue"
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
	// one mention at a time per repository: a burst of comments must not stack runs
	if active, err := ActiveRun(ctx, session.ID); err != nil {
		log.Error("Agent mention: reading the active run of %s failed: %v", repo.FullName(), err)
		return
	} else if active != nil {
		log.Trace("Agent mention: %s is already answering, so this comment waits", repo.FullName())
		return
	}
	message, run, err := SubmitUserMessage(ctx, session, doer.ID, mentionRequest(repo, issue, doer, comment))
	if err != nil {
		log.Error("Agent mention: starting a run for %s#%d failed: %v", repo.FullName(), issue.Index, err)
		return
	}
	// the thread has to hear how this ended: a run that fails, or one whose answer never reached
	// the surface, is otherwise invisible to everyone but a log. The report outlives the request
	// that started it, which ends as soon as the comment is stored.
	reportCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	go func() {
		defer cancel()
		reportMention(reportCtx, message.ID, run.ID, repo, issue)
	}()
}

// reportMention says how a mention run ended, when the model did not say it itself.
func reportMention(ctx context.Context, requestID, runID int64, repo *repo_model.Repository, issue *issues_model.Issue) {
	agent := UserOf(ctx)
	if agent == nil {
		return
	}
	finished := waitForRun(ctx, runID)
	if finished == nil {
		log.Trace("Agent mention: run %d could not be followed", runID)
		return
	}
	said := mentionAnswer(ctx, issue.ID, requestID)
	if finished.Status == agent_model.RunStatusSucceeded && said != "" {
		return // the model answered in the thread itself
	}
	var body string
	switch finished.Status {
	case agent_model.RunStatusFailed:
		body = "I could not finish this one: " + strings.TrimSpace(finished.Error)
	case agent_model.RunStatusCanceled:
		body = "I stopped before answering this one."
	default:
		if said == "" {
			return
		}
		body = said
	}
	if strings.TrimSpace(body) == "" {
		return
	}
	if _, err := issues_service.CreateIssueComment(ctx, agent, repo, issue, body, nil); err != nil {
		log.Error("Agent mention: reporting on %s#%d failed: %v", repo.FullName(), issue.Index, err)
	}
}

// waitForRun waits until the run is no longer active, telling a run that is already over from
// one this process no longer knows.
func waitForRun(ctx context.Context, runID int64) *agent_model.Run {
	for {
		run := &agent_model.Run{}
		has, err := db.GetEngine(ctx).ID(runID).Get(run)
		if err != nil || !has {
			return nil
		}
		if !agent_model.IsRunActive(run.Status) {
			return run
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

// mentionAnswer is what the model wrote in the thread itself, when it did: the comment it made
// after the request, if any.
func mentionAnswer(ctx context.Context, issueID, requestID int64) string {
	messages, err := ListMessages(ctx, issueID, 60)
	if err != nil {
		return ""
	}
	seen := false
	for _, message := range messages {
		if message.ID == requestID {
			seen = true
			continue
		}
		if !seen {
			continue
		}
		if message.Role == agent_model.MessageRoleTool && strings.HasPrefix(message.Content, "gitea_comment_issue") {
			return message.Content
		}
	}
	return ""
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

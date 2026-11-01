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
	"gitea.dev/modules/timeutil"
	"gitea.dev/modules/translation"
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
	if comment == nil {
		return
	}
	requestMention(ctx, doer, repo, issue, comment.Content, mentions)
}

// answerReview answers a review body that names the agent: a review is a request made on the
// surface like any other.
func answerReview(ctx context.Context, pr *issues_model.PullRequest, review *issues_model.Review, _ *issues_model.Comment, mentions []*user_model.User) {
	agent := UserOf(ctx)
	if agent == nil || pr == nil || review == nil || review.Reviewer == nil || review.ReviewerID == agent.ID {
		return
	}
	if err := pr.LoadIssue(ctx); err != nil {
		log.Error("Agent mention: loading the issue of pull request %d failed: %v", pr.ID, err)
		return
	}
	if err := pr.Issue.LoadRepo(ctx); err != nil {
		log.Error("Agent mention: loading the repository of pull request %d failed: %v", pr.ID, err)
		return
	}
	requestMention(ctx, review.Reviewer, pr.Issue.Repo, pr.Issue, review.Content, mentions)
}

// requestMention is one request made on the surface: it starts the run that answers it, tells the
// thread the work began, and follows the run to the end.
func requestMention(ctx context.Context, doer *user_model.User, repo *repo_model.Repository, issue *issues_model.Issue, body string, mentions []*user_model.User) {
	agent := UserOf(ctx)
	if agent == nil || doer == nil || repo == nil || issue == nil {
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
	// one mention at a time per repository, and a handful a minute: a burst of comments must not
	// become a burst of model calls
	recent, err := db.GetEngine(ctx).Where("conversation_id = ?", session.ID).
		And("created_unix > ?", timeutil.TimeStampNow()-timeutil.TimeStamp(setting.Agent.MentionsPerMinute*60)).
		Count(new(agent_model.Run))
	if err != nil {
		log.Error("Agent mention: counting the runs of %s failed: %v", repo.FullName(), err)
		return
	}
	if recent >= int64(setting.Agent.MentionsPerMinute) {
		log.Trace("Agent mention: %s is answering often, so this comment waits", repo.FullName())
		return
	}
	if active, err := ActiveRun(ctx, session.ID); err != nil {
		log.Error("Agent mention: reading the active run of %s failed: %v", repo.FullName(), err)
		return
	} else if active != nil {
		log.Trace("Agent mention: %s is already answering, so this comment waits", repo.FullName())
		return
	}
	message, run, err := SubmitUserMessage(ctx, session, doer.ID, mentionRequest(repo, issue, doer, body))
	if err != nil {
		log.Error("Agent mention: starting a run for %s#%d failed: %v", repo.FullName(), issue.Index, err)
		return
	}
	// the thread is told the agent is working before it can answer, so a reader knows the work
	// started even when the answer takes a while or never comes
	note, err := issues_service.CreateIssueComment(ctx, agent, repo, issue, statusText(doer, "agent.status_working"), nil)
	if err != nil {
		log.Error("Agent mention: saying that a run started on %s#%d failed: %v", repo.FullName(), issue.Index, err)
	} else {
		run.ReplyRepoID, run.ReplyIssueID, run.StatusCommentID = repo.ID, issue.ID, note.ID
		if _, err := db.GetEngine(ctx).ID(run.ID).Cols("reply_repo_id", "reply_issue_id", "status_comment_id").Update(run); err != nil {
			log.Error("Agent mention: recording the status comment of run %d failed: %v", run.ID, err)
		}
	}
	// the thread has to hear how this ended: a run that fails, or one whose answer never reached
	// the surface, is otherwise invisible to everyone but a log. The report outlives the request
	// that started it, which ends as soon as the comment is stored.
	reportCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	go func() {
		defer cancel()
		defer func() {
			// a report is a courtesy to the thread; it must never be the reason the instance stops
			if recovered := recover(); recovered != nil {
				log.Error("Agent mention: reporting run %d panicked: %v", run.ID, recovered)
			}
		}()
		reportMention(reportCtx, message.ID, run.ID, repo, issue)
	}()
}

// statusText is one line in the language of the reader the agent is answering.
func statusText(reader *user_model.User, key string) string {
	lang := "en-US"
	if reader != nil && reader.Language != "" {
		lang = reader.Language
	}
	return translation.NewLocale(lang).TrString(key)
}

// reportMention says how a mention run ended by settling the comment it opened with: the thread
// knows what happened without a new comment for every step.
func reportMention(ctx context.Context, requestID, runID int64, repo *repo_model.Repository, issue *issues_model.Issue) {
	agent := UserOf(ctx)
	if agent == nil {
		return
	}
	finished := waitForRun(ctx, runID, func(active *agent_model.Run) {
		if active.Status == agent_model.RunStatusWaiting {
			waitingText(ctx, active)
			return
		}
		progressText(ctx, active, requestID)
	})
	if finished == nil {
		log.Trace("Agent mention: run %d could not be followed", runID)
		return
	}
	note, answered := endingText(ctx, finished, mentionAnswer(ctx, finished.ConversationID, requestID))
	comment := statusComment(ctx, finished.StatusCommentID)
	if answered {
		// the model answered in the thread itself: the comment that said it was working has done
		// its job and is taken back rather than left standing
		if comment != nil {
			if err := issues_service.DeleteComment(ctx, agent, comment); err != nil {
				log.Error("Agent mention: taking back the status comment of run %d failed: %v", runID, err)
			}
		}
		return
	}
	if note == "" {
		return
	}
	if comment == nil {
		if _, err := issues_service.CreateIssueComment(ctx, agent, repo, issue, note, nil); err != nil {
			log.Error("Agent mention: reporting on %s#%d failed: %v", repo.FullName(), issue.Index, err)
		}
		return
	}
	previous := comment.Content
	comment.Content = note
	if err := issues_service.UpdateComment(ctx, comment, comment.ContentVersion, agent, previous); err != nil {
		log.Error("Agent mention: updating the status comment of run %d failed: %v", runID, err)
		return
	}
	log.Trace("Agent mention: run %d settled its status comment", runID)
}

// endingText is what the thread is left with when a run ends: the failure, the stop, or the
// answer the model kept to itself. answered says the model already wrote in the thread.
func endingText(ctx context.Context, run *agent_model.Run, said string) (text string, answered bool) {
	var reader *user_model.User
	if run.UserID != 0 {
		if user, err := user_model.GetUserByID(ctx, run.UserID); err == nil {
			reader = user
		}
	}
	switch run.Status {
	case agent_model.RunStatusFailed:
		return statusText(reader, "agent.status_failed") + ": " + strings.TrimSpace(run.Error), false
	case agent_model.RunStatusCanceled:
		return statusText(reader, "agent.status_stopped"), false
	case agent_model.RunStatusSucceeded:
		if said != "" {
			return "", true
		}
		return answeredText(ctx, run), false
	}
	return "", false
}

// answeredText is the last thing the model said, for a run whose answer never reached the thread.
func answeredText(ctx context.Context, run *agent_model.Run) string {
	messages, err := ListMessages(ctx, run.ConversationID, 20)
	if err != nil {
		return ""
	}
	for _, message := range slices.Backward(messages) {
		if message.Role == agent_model.MessageRoleAssistant && strings.TrimSpace(message.Content) != "" {
			return strings.TrimSpace(message.Content)
		}
	}
	return ""
}

// waitingText says in the thread that the run waits for its reader, and where to answer it. The
// comment is written once per waiting call, so a reader who comes back later still finds it.
func waitingText(ctx context.Context, run *agent_model.Run) {
	if run.StatusCommentID == 0 {
		return
	}
	call := ""
	pending, err := PendingApprovals(ctx, run.ConversationID)
	if err == nil {
		for _, approval := range pending {
			if approval.RunID == run.ID {
				call = approval.Tool
			}
		}
	}
	// the note names the call that waits now, so a run that waits twice for two different calls
	// says which one it is on
	if call == "" || !markWaitingNote(run.ID, call) {
		return
	}
	agent := UserOf(ctx)
	comment := statusComment(ctx, run.StatusCommentID)
	if agent == nil || comment == nil {
		return
	}
	reader := readerOf(ctx, run)
	note := fmt.Sprintf("%s: %s — [%s](%s/-/agent/approvals/%d)",
		statusText(reader, "agent.status_waiting"), call, statusText(reader, "agent.status_waiting_link"), setting.AppSubURL, approvalID(ctx, run))
	previous := comment.Content
	comment.Content = note
	if err := issues_service.UpdateComment(ctx, comment, comment.ContentVersion, agent, previous); err != nil {
		log.Error("Agent mention: saying what run %d waits for failed: %v", run.ID, err)
	}
}

// progressText says in the thread what the run is reading with, once per tool: a reader can tell
// work from a stall without a comment for every call.
func progressText(ctx context.Context, run *agent_model.Run, requestID int64) {
	if run.StatusCommentID == 0 {
		return
	}
	tool := lastToolOf(ctx, run.ConversationID, requestID)
	if tool == "" || !markProgress(run.ID, tool) {
		return
	}
	agent := UserOf(ctx)
	comment := statusComment(ctx, run.StatusCommentID)
	if agent == nil || comment == nil {
		return
	}
	reader := readerOf(ctx, run)
	previous := comment.Content
	comment.Content = fmt.Sprintf(statusText(reader, "agent.status_working_on"), tool)
	if err := issues_service.UpdateComment(ctx, comment, comment.ContentVersion, agent, previous); err != nil {
		log.Error("Agent mention: saying what run %d reads with failed: %v", run.ID, err)
	}
}

// lastToolOf is the tool the transcript shows the run used last, after the request.
func lastToolOf(ctx context.Context, conversationID, requestID int64) string {
	messages, err := ListMessages(ctx, conversationID, 30)
	if err != nil {
		return ""
	}
	seen := false
	for _, message := range messages {
		if message.ID == requestID {
			seen = true
			continue
		}
		if !seen || message.Role != agent_model.MessageRoleTool {
			continue
		}
		name, _, _ := strings.Cut(message.Content, " ")
		return strings.TrimSpace(name)
	}
	return ""
}

// approvalID is the call a waiting run waits for, so the thread can link to its page.
func approvalID(ctx context.Context, run *agent_model.Run) int64 {
	pending, err := PendingApprovals(ctx, run.ConversationID)
	if err != nil {
		return 0
	}
	for _, approval := range pending {
		if approval.RunID == run.ID {
			return approval.ID
		}
	}
	return 0
}

// markProgress reports whether a run just started reading with a different tool.
func markProgress(runID int64, tool string) bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.progress[runID] == tool {
		return false
	}
	engine.progress[runID] = tool
	return true
}

// markWaitingNote reports whether the waiting note of a run still has to be written: it is
// written once per waiting call, however often the poll looks at the run.
func markWaitingNote(runID int64, call string) bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.waited[runID] == call {
		return false
	}
	engine.waited[runID] = call
	return true
}

// statusComment is the comment a run says how it is going in, when it still stands.
func statusComment(ctx context.Context, id int64) *issues_model.Comment {
	if id == 0 {
		return nil
	}
	comment, err := issues_model.GetCommentByID(ctx, id)
	if err != nil {
		return nil
	}
	return comment
}

// waitForRun waits until the run is no longer active, telling a run that is already over from
// one this process no longer knows. A run that waits for its reader is handed to onWaiting, so
// the thread can say what it waits for; the reader may come back hours later.
func waitForRun(ctx context.Context, runID int64, onActive func(*agent_model.Run)) *agent_model.Run {
	for {
		run := &agent_model.Run{}
		has, err := db.GetEngine(ctx).ID(runID).Get(run)
		if err != nil || !has {
			return nil
		}
		if agent_model.IsRunActive(run.Status) && onActive != nil {
			onActive(run)
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
func mentionAnswer(ctx context.Context, conversationID, requestID int64) string {
	messages, err := ListMessages(ctx, conversationID, 60)
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
func mentionRequest(repo *repo_model.Repository, issue *issues_model.Issue, doer *user_model.User, body string) string {
	builder := strings.Builder{}
	fmt.Fprintf(&builder, "[mention] @%s was named by @%s in a comment on %s #%d, %q.\n",
		setting.Agent.Username, doer.Name, repo.FullName(), issue.Index, issue.Title)
	fmt.Fprintf(&builder, "Answer in that thread with gitea_comment_issue (repository %s, index %d) once you have something to say; "+
		"gitea_read_issue and gitea_read_pull_request read what it is about.\n", repo.FullName(), issue.Index)
	builder.WriteString(strings.TrimSpace(body))
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

// settleInterruptedRun leaves the failure in the thread a dead run opened with a status comment.
func settleInterruptedRun(ctx context.Context, run *agent_model.Run) {
	if run.StatusCommentID == 0 {
		return
	}
	agent := UserOf(ctx)
	if agent == nil {
		return
	}
	comment, err := issues_model.GetCommentByID(ctx, run.StatusCommentID)
	if err != nil {
		return
	}
	previous := comment.Content
	comment.Content = statusText(readerOf(ctx, run), "agent.status_interrupted")
	if err := issues_service.UpdateComment(ctx, comment, comment.ContentVersion, agent, previous); err != nil {
		log.Error("Agent engine: settling the status comment of run %d failed: %v", run.ID, err)
	}
}

// InitMentions makes the agent answer the comments that name it. It is called once, at start
// up, after the engine has its workers.
func InitMentions() {
	if setting.Agent.Username == "" {
		return
	}
	notify_service.CommentListener = answerMention
	notify_service.ReviewListener = answerReview
}

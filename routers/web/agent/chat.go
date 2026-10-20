// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

const (
	tplChat = "agent/chat"
	chatURL = "/-/agent"
)

// chatTitle uses the submitted title, falling back to the localized name of the kind.
func chatTitle(ctx *context.Context, localeKey string) string {
	if title := ctx.FormTrim("title"); title != "" {
		return title
	}
	return string(ctx.Tr(localeKey))
}

func chatSessionURL(id int64) string {
	return fmt.Sprintf("%s%s/%d", setting.AppSubURL, chatURL, id)
}

func chatError(ctx *context.Context, err error) {
	switch {
	case errors.Is(err, agent_service.ErrConversationNotFound{}), errors.Is(err, agent_service.ErrConversationDenied{}):
		// a session the doer may not use must not be distinguishable from a missing one
		ctx.NotFound(nil)
	default:
		ctx.ServerError("agent chat", err)
	}
}

// Chat renders the chat screen with the session list and no session open. Small screens
// use this as the session list page.
func Chat(ctx *context.Context) {
	renderChat(ctx, nil)
}

// ChatSession renders the chat screen with one session open.
func ChatSession(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	renderChat(ctx, session)
}

func renderChat(ctx *context.Context, session *agent_model.Conversation) {
	list, err := agent_service.ListConversations(ctx, ctx.Doer)
	if err != nil {
		ctx.ServerError("ListConversations", err)
		return
	}
	repos, err := agent_service.WritableRepositories(ctx, ctx.Doer, 50)
	if err != nil {
		ctx.ServerError("WritableRepositories", err)
		return
	}

	options, err := agent_service.ModelOptions(ctx, 200)
	if err != nil {
		ctx.ServerError("ModelOptions", err)
		return
	}
	ctx.Data["Title"] = ctx.Tr("agent.chat_title")
	ctx.Data["PageIsAgentChat"] = true
	ctx.Data["Sessions"] = list
	ctx.Data["Session"] = session
	ctx.Data["WritableRepos"] = repos
	ctx.Data["ModelOptions"] = options
	if session != nil {
		if session.Kind == agent_model.ConversationKindRepository {
			repo, err := repo_model.GetRepositoryByID(ctx, session.RepoID)
			if err != nil {
				ctx.ServerError("GetRepositoryByID", err)
				return
			}
			ctx.Data["Repo"] = repo
		}
		messages, err := agent_service.ListMessages(ctx, session.ID)
		if err != nil {
			ctx.ServerError("ListMessages", err)
			return
		}
		ctx.Data["Messages"] = messages
		ctx.Data["LastMessageID"] = lastMessageID(messages)
		ctx.Data["ModelRefLabel"] = agent_service.ModelRefLabel(ctx, session.ModelRef)
		run, err := agent_service.LatestRun(ctx, session.ID)
		if err != nil {
			ctx.ServerError("LatestRun", err)
			return
		}
		labels := map[string]string{
			"queued":   string(ctx.Tr("agent.chat_status_queued")),
			"running":  string(ctx.Tr("agent.chat_status_running")),
			"failed":   string(ctx.Tr("agent.chat_status_failed")),
			"canceled": string(ctx.Tr("agent.chat_status_canceled")),
			"cancel":   string(ctx.Tr("agent.chat_cancel")),
			"error":    string(ctx.Tr("agent.chat_status_error")),
		}
		labelsJSON, err := json.Marshal(labels)
		if err != nil {
			ctx.ServerError("LabelsJSON", err)
			return
		}
		ctx.Data["LabelsJSON"] = string(labelsJSON)
		ctx.Data["RunActive"] = run != nil && agent_model.IsRunActive(run.Status)
		ctx.Data["RunCancelable"] = run != nil && agent_model.IsRunActive(run.Status)
		ctx.Data["RunText"] = runText(run, labels)
	}
	ctx.HTML(http.StatusOK, tplChat)
}

// ChatSessionNew starts a server or repository session and opens it.
func ChatSessionNew(ctx *context.Context) {
	kind := ctx.FormString("kind")
	switch kind {
	case agent_model.ConversationKindServer:
		if !ctx.Doer.IsAdmin {
			ctx.NotFound(nil)
			return
		}
		session, err := agent_service.CreateConversation(ctx, kind, 0, chatTitle(ctx, "agent.chat_server"))
		if err != nil {
			ctx.ServerError("CreateConversation", err)
			return
		}
		ctx.Redirect(chatSessionURL(session.ID))
	case agent_model.ConversationKindRepository:
		repo, err := repo_model.GetRepositoryByID(ctx, ctx.FormInt64("repo_id"))
		if err != nil {
			ctx.ServerError("GetRepositoryByID", err)
			return
		}
		if repo == nil {
			ctx.Flash.Error(ctx.Tr("agent.chat_repo_required"))
			ctx.Redirect(setting.AppSubURL + chatURL)
			return
		}
		allowed, err := agent_service.CanUseRepository(ctx, ctx.Doer, repo)
		if err != nil {
			ctx.ServerError("CanUseRepository", err)
			return
		}
		if !allowed {
			ctx.NotFound(nil)
			return
		}
		session, err := agent_service.CreateConversation(ctx, kind, repo.ID, chatTitle(ctx, "agent.chat_repository"))
		if err != nil {
			ctx.ServerError("CreateConversation", err)
			return
		}
		ctx.Redirect(chatSessionURL(session.ID))
	default:
		ctx.NotFound(nil)
	}
}

// ChatSessionDelete removes a session and returns to the session list.
func ChatSessionDelete(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	if err := agent_service.DeleteConversation(ctx, session); err != nil {
		if errors.Is(err, agent_service.ErrResidentUndeletable{}) {
			ctx.Flash.Error(ctx.Tr("agent.chat_resident_undeletable"))
		} else {
			ctx.Flash.Error(err.Error())
		}
	} else {
		ctx.Flash.Success(ctx.Tr("agent.chat_deleted"))
	}
	ctx.Redirect(setting.AppSubURL + chatURL)
}

// ChatMessageNew stores a user message and starts a run. The answer is produced on the
// server, so the client may leave right after sending.
func ChatMessageNew(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	if _, err := agent_service.SubmitUserMessage(ctx, session, ctx.FormString("content")); err != nil {
		if isXMLHTTP(ctx) {
			ctx.Resp.WriteHeader(http.StatusBadRequest)
			return
		}
		ctx.Flash.Error(ctx.Tr("agent.chat_message_empty"))
		ctx.Redirect(chatSessionURL(session.ID))
		return
	}
	answer(ctx, session.ID)
}

// ChatRunCancel stops the run of a session.
func ChatRunCancel(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	if err := agent_service.CancelRun(session.ID); err != nil && !errors.Is(err, agent_service.ErrNoActiveRun{}) {
		ctx.ServerError("CancelRun", err)
		return
	}
	answer(ctx, session.ID)
}

// ChatModelSet binds a session to the model it answers with.
func ChatModelSet(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	ref := ctx.FormTrim("model_ref")
	allowed, err := agent_service.CanUseModelRef(ctx, ref)
	if err != nil {
		ctx.ServerError("CanUseModelRef", err)
		return
	}
	if !allowed {
		ctx.Flash.Error(ctx.Tr("agent.chat_model_invalid"))
		ctx.Redirect(chatSessionURL(session.ID))
		return
	}
	if err := agent_service.SetConversationModel(ctx, session, ref); err != nil {
		ctx.ServerError("SetConversationModel", err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.chat_model_saved"))
	ctx.Redirect(chatSessionURL(session.ID))
}

// runText renders the status strip text of a run; succeeded runs show nothing.
func runText(run *agent_model.Run, labels map[string]string) string {
	if run == nil {
		return ""
	}
	switch run.Status {
	case agent_model.RunStatusQueued:
		return labels["queued"]
	case agent_model.RunStatusRunning:
		return labels["running"]
	case agent_model.RunStatusCanceled:
		return labels["canceled"]
	case agent_model.RunStatusFailed:
		if run.Error == "" {
			return labels["failed"]
		}
		return labels["failed"] + ": " + run.Error
	}
	return ""
}

// answer replies to a request that may or may not be an XHR.
func answer(ctx *context.Context, conversationID int64) {
	if isXMLHTTP(ctx) {
		ctx.Resp.WriteHeader(http.StatusNoContent)
		return
	}
	ctx.Redirect(chatSessionURL(conversationID))
}

func isXMLHTTP(ctx *context.Context) bool {
	return ctx.Req.Header.Get("X-Requested-With") == "XMLHttpRequest"
}

// lastMessageID returns the newest transcript entry ID, which clients resume from.
func lastMessageID(messages []*agent_model.Message) int64 {
	if len(messages) == 0 {
		return 0
	}
	return messages[len(messages)-1].ID
}

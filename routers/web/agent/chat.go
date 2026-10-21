// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	scope, scopeBaseURL := chatScope(ctx, session, list)
	keyword := ctx.FormTrim("q")
	if keyword != "" {
		list = filterConversations(list, keyword)
	}
	ctx.Data["Keyword"] = keyword
	ctx.Data["Sessions"] = list
	ctx.Data["Scope"] = scope
	ctx.Data["ScopeBaseURL"] = scopeBaseURL
	ctx.Data["ScopeCounts"] = map[string]int{
		"pod":        boolInt(list.Resident != nil),
		"server":     len(list.Servers),
		"repository": repositorySessionCount(list),
	}
	ctx.Data["Session"] = session
	ctx.Data["WritableRepos"] = repos
	ctx.Data["ModelOptions"] = options
	ctx.Data["ConversationIcons"] = agent_service.ConversationIcons()
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

// ChatDraft renders the chat screen for a conversation that does not exist yet: the first
// message creates it.
func ChatDraft(ctx *context.Context) {
	kind := agent_model.ConversationKindServer
	var repo *repo_model.Repository
	if repoID := ctx.FormInt64("repo_id"); repoID != 0 && ctx.Doer.IsAdmin {
		kind = agent_model.ConversationKindRepository
		var err error
		if repo, err = repo_model.GetRepositoryByID(ctx, repoID); err != nil {
			ctx.ServerError("GetRepositoryByID", err)
			return
		}
		if repo == nil {
			ctx.NotFound(nil)
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
	}
	renderChat(ctx, nil)
	ctx.Data["IsDraft"] = true
	ctx.Data["DraftKind"] = kind
	ctx.Data["Repo"] = repo
	ctx.Data["DraftModelRef"] = draftModelRef(ctx)
	ctx.HTML(http.StatusOK, tplChat)
}

// draftModelRef preselects the model of the resident session, so a new conversation usually
// needs no extra choice.
func draftModelRef(ctx *context.Context) string {
	resident, err := agent_service.EnsureResidentConversation(ctx)
	if err != nil {
		return ""
	}
	return resident.ModelRef
}

// ChatDraftCreate stores the conversation of a draft along with its first message.
func ChatDraftCreate(ctx *context.Context) {
	kind := ctx.FormString("kind")
	repoID := ctx.FormInt64("repo_id")
	var repo *repo_model.Repository
	switch kind {
	case agent_model.ConversationKindRepository:
		if !ctx.Doer.IsAdmin {
			ctx.NotFound(nil)
			return
		}
		var err error
		if repo, err = repo_model.GetRepositoryByID(ctx, repoID); err != nil {
			ctx.ServerError("GetRepositoryByID", err)
			return
		}
		if repo == nil {
			ctx.NotFound(nil)
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
	case agent_model.ConversationKindServer:
		if !ctx.Doer.IsAdmin {
			ctx.NotFound(nil)
			return
		}
	default:
		ctx.NotFound(nil)
		return
	}

	modelRef := ctx.FormTrim("model_ref")
	if modelRef == "" {
		ctx.Flash.Error(ctx.Tr("agent.chat_model_required"))
		ctx.Redirect(setting.AppSubURL + chatURL + "/new")
		return
	}
	allowed, err := agent_service.CanUseModelRef(ctx, modelRef)
	if err != nil {
		ctx.ServerError("CanUseModelRef", err)
		return
	}
	if !allowed {
		ctx.Flash.Error(ctx.Tr("agent.chat_model_invalid"))
		ctx.Redirect(setting.AppSubURL + chatURL + "/new")
		return
	}

	session, err := agent_service.CreateConversation(ctx, kind, repoID, chatTitle(ctx, "agent.chat_new_conversation"))
	if err != nil {
		ctx.ServerError("CreateConversation", err)
		return
	}
	if err := agent_service.SetConversationModel(ctx, session, modelRef); err != nil {
		ctx.ServerError("SetConversationModel", err)
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
	if isXMLHTTP(ctx) {
		ctx.Resp.Header().Set("Location", chatSessionURL(session.ID))
		ctx.Resp.WriteHeader(http.StatusNoContent)
		return
	}
	ctx.Redirect(chatSessionURL(session.ID))
}

// ChatFolderCreate stores a folder for the conversation list.
func ChatFolderCreate(ctx *context.Context) {
	if _, err := agent_service.CreateConversationFolder(ctx, ctx.FormTrim("name"), ctx.FormTrim("icon")); err != nil {
		ctx.Flash.Error(folderErrorText(ctx, err))
	} else {
		ctx.Flash.Success(ctx.Tr("agent.chat_folder_added"))
	}
	redirectToChatList(ctx)
}

// ChatFolderUpdate renames a folder or changes its icon.
func ChatFolderUpdate(ctx *context.Context) {
	if _, err := agent_service.UpdateConversationFolder(ctx, ctx.PathParamInt64("id"), ctx.FormTrim("name"), ctx.FormTrim("icon")); err != nil {
		ctx.Flash.Error(folderErrorText(ctx, err))
	} else {
		ctx.Flash.Success(ctx.Tr("agent.chat_folder_saved"))
	}
	redirectToChatList(ctx)
}

// ChatFolderDelete removes a folder; its conversations go back to the root list.
func ChatFolderDelete(ctx *context.Context) {
	if err := agent_service.DeleteConversationFolder(ctx, ctx.PathParamInt64("id")); err != nil {
		if errors.Is(err, agent_service.ErrConversationFolderNotFound{}) {
			ctx.NotFound(nil)
			return
		}
		ctx.ServerError("DeleteConversationFolder", err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.chat_folder_deleted"))
	redirectToChatList(ctx)
}

// ChatSessionFolder files a conversation under a folder, or moves it back to the root.
func ChatSessionFolder(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	if err := agent_service.SetConversationFolder(ctx, session, ctx.FormInt64("folder_id")); err != nil {
		ctx.Flash.Error(folderErrorText(ctx, err))
	} else {
		ctx.Flash.Success(ctx.Tr("agent.chat_folder_moved"))
	}
	redirectToChatList(ctx)
}

func folderErrorText(ctx *context.Context, err error) string {
	if errors.Is(err, agent_service.ErrConversationFolderInvalid{}) {
		return string(ctx.Tr("agent.chat_folder_name_required"))
	}
	if errors.Is(err, agent_service.ErrConversationFolderNotFound{}) {
		return string(ctx.Tr("agent.chat_folder_missing"))
	}
	return err.Error()
}

func redirectToChatList(ctx *context.Context) {
	ctx.Redirect(setting.AppSubURL + chatURL)
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

// ChatTitlePost renames a session.
func ChatTitlePost(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	if err := agent_service.SetConversationTitle(ctx, session, ctx.FormString("title")); err != nil {
		ctx.Flash.Error(ctx.Tr("agent.chat_title_required"))
		ctx.Redirect(chatSessionURL(session.ID))
		return
	}
	agent_service.NotifyConversation(session)
	ctx.Flash.Success(ctx.Tr("agent.chat_title_saved"))
	ctx.Redirect(chatSessionURL(session.ID))
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

// chatScope returns the session group the sidebar shows: the one asked for, else the one
// the open session belongs to, else Pod.
func chatScope(ctx *context.Context, session *agent_model.Conversation, list *agent_service.ConversationList) (string, string) {
	baseURL := setting.AppSubURL + chatURL
	if session != nil {
		baseURL = chatSessionURL(session.ID)
	}

	switch ctx.FormString("scope") {
	case "pod", "server":
		// server agents are sessions of the global agent, so they live under Pod
		return "pod", baseURL
	case "repository":
		return "repository", baseURL
	}
	if session != nil && session.Kind == agent_model.ConversationKindRepository {
		return "repository", baseURL
	}
	return "pod", baseURL
}

// filterConversations keeps the sessions whose title matches, and the repositories whose
// name matches. The resident session is kept when its title matches.
func filterConversations(list *agent_service.ConversationList, keyword string) *agent_service.ConversationList {
	match := func(text string) bool {
		return strings.Contains(strings.ToLower(text), strings.ToLower(keyword))
	}

	filtered := &agent_service.ConversationList{}
	if list.Resident != nil && match(list.Resident.Title) {
		filtered.Resident = list.Resident
	}
	for _, session := range list.Servers {
		if match(session.Title) {
			filtered.Servers = append(filtered.Servers, session)
		}
	}
	for _, group := range list.Repositories {
		repoMatches := match(group.Repo.FullName())
		kept := &agent_service.RepositoryConversations{Repo: group.Repo}
		for _, session := range group.Sessions {
			if repoMatches || match(session.Title) {
				kept.Sessions = append(kept.Sessions, session)
			}
		}
		if len(kept.Sessions) > 0 {
			filtered.Repositories = append(filtered.Repositories, kept)
		}
	}
	return filtered
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func repositorySessionCount(list *agent_service.ConversationList) int {
	count := 0
	for _, group := range list.Repositories {
		count += len(group.Sessions)
	}
	return count
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

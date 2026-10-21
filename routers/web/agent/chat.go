// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/httplib"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

const (
	tplChat = "agent/chat"
	chatURL = "/-/agent"

	// chatMessagePageSize is how much of a transcript the chat screen loads at once.
	chatMessagePageSize = 50
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
	prepareChat(ctx, nil)
	ctx.HTML(http.StatusOK, tplChat)
}

// ChatSession renders the chat screen with one session open.
func ChatSession(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	prepareChat(ctx, session)
	ctx.HTML(http.StatusOK, tplChat)
}

// prepareChat fills the page data of the chat screen. The caller renders once.
func prepareChat(ctx *context.Context, session *agent_model.Conversation) {
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
		messages, err := agent_service.ListMessages(ctx, session.ID, chatMessagePageSize)
		if err != nil {
			ctx.ServerError("ListMessages", err)
			return
		}
		oldestID := int64(0)
		if len(messages) > 0 {
			oldestID = messages[0].ID
		}
		hasOlder, err := agent_service.HasMessagesBefore(ctx, session.ID, oldestID)
		if err != nil {
			ctx.ServerError("HasMessagesBefore", err)
			return
		}
		messageIDs := make([]int64, 0, len(messages))
		for _, message := range messages {
			messageIDs = append(messageIDs, message.ID)
		}
		attachments, err := agent_service.ListMessageAttachments(ctx, messageIDs)
		if err != nil {
			ctx.ServerError("ListMessageAttachments", err)
			return
		}
		ctx.Data["Messages"] = messages
		ctx.Data["MessageAttachments"] = attachments
		ctx.Data["OldestMessageID"] = oldestID
		ctx.Data["HasOlderMessages"] = hasOlder
		ctx.Data["LastMessageID"] = lastMessageID(messages)
		role := agent_service.ModelRoleForKind(session.Kind)
		ref, err := agent_service.ModelRoleRef(ctx, role)
		if err != nil {
			ctx.ServerError("ModelRoleRef", err)
			return
		}
		ctx.Data["RoleModelLabel"] = agent_service.ModelRefLabel(ctx, ref)
		ctx.Data["CanAnswer"] = ref != ""
		run, err := agent_service.LatestRun(ctx, session.ID)
		if err != nil {
			ctx.ServerError("LatestRun", err)
			return
		}
		labels := map[string]string{
			"queued":    string(ctx.Tr("agent.chat_status_queued")),
			"running":   string(ctx.Tr("agent.chat_status_running")),
			"failed":    string(ctx.Tr("agent.chat_status_failed")),
			"canceled":  string(ctx.Tr("agent.chat_status_canceled")),
			"cancel":    string(ctx.Tr("agent.chat_cancel")),
			"copy":      string(ctx.Tr("agent.chat_copy")),
			"edit":      string(ctx.Tr("agent.chat_edit")),
			"error":     string(ctx.Tr("agent.chat_status_error")),
			"reasoning": string(ctx.Tr("agent.chat_reasoning")),
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
	prepareChat(ctx, nil)
	ctx.Data["IsDraft"] = true
	ctx.Data["DraftKind"] = kind
	ctx.Data["Repo"] = repo
	role := agent_service.ModelRoleForKind(kind)
	ref, err := agent_service.ModelRoleRef(ctx, role)
	if err != nil {
		ctx.ServerError("ModelRoleRef", err)
		return
	}
	ctx.Data["RoleModelLabel"] = agent_service.ModelRefLabel(ctx, ref)
	ctx.Data["CanAnswer"] = ref != ""
	ctx.HTML(http.StatusOK, tplChat)
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

	session, err := agent_service.CreateConversation(ctx, kind, repoID, chatTitle(ctx, "agent.chat_new_conversation"))
	if err != nil {
		ctx.ServerError("CreateConversation", err)
		return
	}
	message, _, err := agent_service.SubmitUserMessage(ctx, session, ctx.FormString("content"))
	if err != nil {
		if isXMLHTTP(ctx) {
			ctx.Resp.WriteHeader(http.StatusBadRequest)
			return
		}
		ctx.Flash.Error(ctx.Tr("agent.chat_message_empty"))
		ctx.Redirect(chatSessionURL(session.ID))
		return
	}
	storeUploads(ctx, session, message)
	if isXMLHTTP(ctx) {
		ctx.Resp.Header().Set("Location", chatSessionURL(session.ID))
		ctx.Resp.WriteHeader(http.StatusNoContent)
		return
	}
	ctx.Redirect(chatSessionURL(session.ID))
}

// storeUploads saves the files sent with a message. A rejected file only fails itself.
func storeUploads(ctx *context.Context, conversation *agent_model.Conversation, message *agent_model.Message) {
	if !agent_service.AttachmentAllowed() || ctx.Req.MultipartForm == nil {
		return
	}
	for _, header := range ctx.Req.MultipartForm.File["files"] {
		file, err := header.Open()
		if err != nil {
			ctx.Flash.Error(ctx.Tr("agent.chat_attachment_failed", header.Filename, err.Error()))
			continue
		}
		_, err = agent_service.StoreAttachment(ctx, conversation.ID, message.ID, header.Filename, file, header.Size)
		_ = file.Close()
		if err != nil {
			ctx.Flash.Error(ctx.Tr("agent.chat_attachment_failed", header.Filename, err.Error()))
		}
	}
}

// ChatAttachment serves a file sent with a message.
func ChatAttachment(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	attachment, err := agent_service.GetAttachment(ctx, session.ID, ctx.PathParamInt64("aid"))
	if err != nil {
		if errors.Is(err, agent_service.ErrAttachmentNotFound{}) {
			ctx.NotFound(nil)
			return
		}
		ctx.ServerError("GetAttachment", err)
		return
	}
	file, err := os.Open(agent_service.AttachmentPath(attachment))
	if err != nil {
		ctx.NotFound(nil)
		return
	}
	defer file.Close()
	ctx.ServeContent(file, context.ServeHeaderOptions{
		Filename:           attachment.Name,
		ContentDisposition: httplib.ContentDispositionAttachment,
		LastModified:       attachment.CreatedUnix.AsLocalTime(),
	})
}

// attachmentPayloads renders the files of a message for the clients.
func attachmentPayloads(attachments []*agent_model.Attachment) []attachmentEvent {
	payloads := make([]attachmentEvent, 0, len(attachments))
	for _, attachment := range attachments {
		payloads = append(payloads, attachmentEvent{
			ID:       attachment.ID,
			Name:     attachment.Name,
			SizeText: attachment.SizeText(),
			URL:      setting.AppSubURL + attachment.URL(),
		})
	}
	return payloads
}

// ChatMessagesBefore returns one page of older messages as JSON, in the same shape the
// event stream uses, so the client renders both the same way.
func ChatMessagesBefore(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	before := ctx.FormInt64("before")
	if before == 0 {
		ctx.JSON(http.StatusOK, []messageEvent{})
		return
	}
	messages, err := agent_service.ListMessagesBefore(ctx, session.ID, before, chatMessagePageSize)
	if err != nil {
		ctx.ServerError("ListMessagesBefore", err)
		return
	}
	response := make([]messageEvent, 0, len(messages))
	for _, message := range messages {
		response = append(response, messageEventOf(ctx, message))
	}
	ctx.JSON(http.StatusOK, response)
}

// ChatExport returns the whole transcript as markdown: it is what "copy the conversation"
// and "download" hand to the browser.
func ChatExport(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	messages, err := agent_service.ListAllMessages(ctx, session.ID)
	if err != nil {
		ctx.ServerError("ListAllMessages", err)
		return
	}

	var builder strings.Builder
	builder.WriteString("# " + session.Title + "\n\n")
	for _, message := range messages {
		builder.WriteString("## " + messageRoleLabel(ctx, message.Role) + "\n\n")
		builder.WriteString(strings.TrimSpace(message.Content) + "\n\n")
	}
	ctx.Resp.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	ctx.Resp.Header().Set("Content-Disposition", "attachment; filename=\"conversation.md\"")
	_, _ = ctx.Resp.Write([]byte(builder.String()))
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
	if err := agent_service.DeleteConversationAttachments(ctx, session.ID); err != nil {
		ctx.ServerError("DeleteConversationAttachments", err)
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
	message, _, err := agent_service.SubmitUserMessage(ctx, session, ctx.FormString("content"))
	if err != nil {
		if isXMLHTTP(ctx) {
			ctx.Resp.WriteHeader(http.StatusBadRequest)
			return
		}
		ctx.Flash.Error(ctx.Tr("agent.chat_message_empty"))
		ctx.Redirect(chatSessionURL(session.ID))
		return
	}
	storeUploads(ctx, session, message)
	answer(ctx, session.ID)
}

// ChatMessageEdit rewrites a user message and asks the agent again from that point.
func ChatMessageEdit(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	if _, err := agent_service.EditUserMessage(ctx, session, ctx.PathParamInt64("mid"), ctx.FormString("content")); err != nil {
		switch {
		case errors.Is(err, agent_service.ErrMessageNotEditable{}):
			ctx.Flash.Error(ctx.Tr("agent.chat_edit_only_user"))
		case errors.Is(err, agent_service.ErrConversationNotFound{}):
			ctx.NotFound(nil)
			return
		default:
			ctx.Flash.Error(ctx.Tr("agent.chat_message_empty"))
		}
		ctx.Redirect(chatSessionURL(session.ID))
		return
	}
	if _, err := agent_service.StartRun(ctx, session); err != nil {
		ctx.ServerError("StartRun", err)
		return
	}
	ctx.Flash.Success(ctx.Tr("agent.chat_edited"))
	ctx.Redirect(chatSessionURL(session.ID))
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
		if errors.Is(err, agent_service.ErrConversationImmutable{}) {
			ctx.Flash.Error(ctx.Tr("agent.chat_resident_immutable"))
		} else {
			ctx.Flash.Error(ctx.Tr("agent.chat_title_required"))
		}
		ctx.Redirect(chatSessionURL(session.ID))
		return
	}
	agent_service.NotifyConversation(session)
	ctx.Flash.Success(ctx.Tr("agent.chat_title_saved"))
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

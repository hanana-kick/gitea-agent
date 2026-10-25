// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/httplib"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

const (
	tplChat = "agent/chat"
	chatURL = "/-/agent"

	// chatMessagePageSize is how much of a transcript the chat screen loads at once.
	chatMessagePageSize = 50

	// chatAttachmentLimit is how many files of one kind a message shows before the rest
	// waits behind a "more" button. Keep it in step with attachmentLimit in agent-chat.ts.
	chatAttachmentLimit = 5
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
	prepareChat(ctx, nil, nil)
	ctx.HTML(http.StatusOK, tplChat)
}

// ChatSession renders the chat screen with one session open.
func ChatSession(ctx *context.Context) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	prepareChat(ctx, session, nil)
	ctx.HTML(http.StatusOK, tplChat)
}

// prepareChat fills the page data of the chat screen. The caller renders once.
func prepareChat(ctx *context.Context, session *agent_model.Conversation, repo *repo_model.Repository) {
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

	// a repository session, and the draft of one, belong to that repository: the list beside
	// them shows its sessions, and "all=1" asks for every repository instead
	if session != nil && session.Kind == agent_model.ConversationKindRepository {
		sessionRepo, err := repo_model.GetRepositoryByID(ctx, session.RepoID)
		if err != nil {
			ctx.ServerError("GetRepositoryByID", err)
			return
		}
		repo = sessionRepo
	}
	if repo != nil {
		ctx.Data["Repo"] = repo
		if ctx.FormString("all") != "1" {
			list = keepRepository(list, repo.ID)
			ctx.Data["FilterRepo"] = repo
		} else {
			ctx.Data["AllRepositories"] = true
		}
	}

	ctx.Data["Title"] = ctx.Tr("agent.chat_title")
	ctx.Data["PageIsAgentChat"] = true
	scope, scopeBaseURL := chatScope(ctx, session, repo)
	keyword := ctx.FormTrim("q")
	if keyword != "" {
		list = filterConversations(list, keyword)
	}
	ctx.Data["Keyword"] = keyword
	ctx.Data["Sessions"] = list
	busy, err := agent_service.BusyConversationIDs(ctx)
	if err != nil {
		ctx.ServerError("BusyConversationIDs", err)
		return
	}
	ctx.Data["BusySessions"] = busy
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
		ctx.Data["MessageAttachments"] = groupAttachments(attachments)
		ctx.Data["ToolRecords"] = toolRecordMap(messages)
		ctx.Data["OldestMessageID"] = oldestID
		ctx.Data["HasOlderMessages"] = hasOlder
		ctx.Data["LastMessageID"] = lastMessageID(messages)
		approvals, err := agent_service.PendingApprovals(ctx, session.ID)
		if err != nil {
			ctx.ServerError("PendingApprovals", err)
			return
		}
		ctx.Data["PendingApprovals"] = approvals
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
			"queued":    string(ctx.Tr("agent.chat_pending_queued")),
			"running":   string(ctx.Tr("agent.chat_pending_running")),
			"failed":    string(ctx.Tr("agent.chat_status_failed")),
			"canceled":  string(ctx.Tr("agent.chat_status_canceled")),
			"copy":      string(ctx.Tr("agent.chat_copy")),
			"edit":      string(ctx.Tr("agent.chat_edit")),
			"error":     string(ctx.Tr("agent.chat_status_error")),
			"reasoning": string(ctx.Tr("agent.chat_reasoning")),
			"thought":   string(ctx.Tr("agent.chat_thought")),
		}
		labelsJSON, err := json.Marshal(labels)
		if err != nil {
			ctx.ServerError("LabelsJSON", err)
			return
		}
		ctx.Data["LabelsJSON"] = string(labelsJSON)
		ctx.Data["RunActive"] = run != nil && agent_model.IsRunActive(run.Status)
		ctx.Data["RunCancelable"] = run != nil && agent_model.IsRunActive(run.Status)
		ctx.Data["PendingState"], ctx.Data["PendingText"] = pendingState(run, labels)
		ctx.Data["PendingRunID"] = int64(0)
		if run != nil {
			ctx.Data["PendingRunID"] = run.ID
		}
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
	prepareChat(ctx, nil, repo)
	ctx.Data["IsDraft"] = true
	ctx.Data["DraftKind"] = kind
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

// RepoAgent renders the pod of one repository: its sessions and the composer for the next one.
// It is the screen the repository menu opens, so the reader stays in the repository's own path.
func RepoAgent(ctx *context.Context) {
	repo := ctx.Repo.Repository
	// an agent of one repository is for the users who may drive it, as in the picker
	if !ctx.Doer.IsAdmin {
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
	prepareChat(ctx, nil, repo)
	ctx.Data["IsDraft"] = true
	ctx.Data["DraftKind"] = agent_model.ConversationKindRepository
	ref, err := agent_service.ModelRoleRef(ctx, agent_service.ModelRoleForKind(agent_model.ConversationKindRepository))
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

	session, err := agent_service.CreateConversation(ctx, kind, ctx.Doer.ID, repoID, chatTitle(ctx, "agent.chat_new_conversation"))
	if err != nil {
		ctx.ServerError("CreateConversation", err)
		return
	}
	message, _, err := agent_service.SubmitUserMessage(ctx, session, ctx.Doer.ID, ctx.FormString("content"))
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
	// a picture opens as itself, so the thumbnail of the transcript leads to a preview;
	// every other file is meant to land in the reader's downloads
	options := context.ServeHeaderOptions{
		Filename:           attachment.Name,
		ContentDisposition: httplib.ContentDispositionAttachment,
		LastModified:       attachment.CreatedUnix.AsLocalTime(),
	}
	if pictureMime := agent_model.PictureMime(attachment.Name); pictureMime != "" {
		options.ContentType = pictureMime
		options.ContentDisposition = httplib.ContentDispositionInline
	}
	ctx.ServeContent(file, options)
}

// attachmentRow is one row of a message's attachments: the files, how many of them the row
// shows and how many wait behind its control.
type attachmentRow struct {
	Files []*agent_model.Attachment
	Shown int
	Rest  int
}

// attachmentGroups is the files of one message split the way the transcript shows them:
// pictures read as a row of thumbnails, everything else as a list of files below it.
type attachmentGroups struct {
	Pictures attachmentRow
	Files    attachmentRow
}

// groupAttachments splits the files of every message of a transcript for the template.
func groupAttachments(attachments map[int64][]*agent_model.Attachment) map[int64]*attachmentGroups {
	grouped := make(map[int64]*attachmentGroups, len(attachments))
	for messageID, files := range attachments {
		group := &attachmentGroups{}
		for _, file := range files {
			if file.IsPicture() {
				group.Pictures.Files = append(group.Pictures.Files, file)
				continue
			}
			group.Files.Files = append(group.Files.Files, file)
		}
		// the picture row keeps one tile for the count of what it holds back
		group.Pictures.Shown, group.Pictures.Rest = attachmentFit(len(group.Pictures.Files), chatAttachmentLimit, true)
		group.Files.Shown, group.Files.Rest = attachmentFit(len(group.Files.Files), chatAttachmentLimit, false)
		grouped[messageID] = group
	}
	return grouped
}

// attachmentFit reports how many files a row shows and how many are left. A row that fits
// shows everything; a longer one stops at limit, one earlier when its last slot is the count.
func attachmentFit(count, limit int, counted bool) (int, int) {
	if count <= limit {
		return count, 0
	}
	shown := limit
	if counted {
		shown--
	}
	return shown, count - shown
}

// toolRecordMap splits the tool records of a transcript for the template: the call the
// model made, and what came back behind it.
func toolRecordMap(messages []*agent_model.Message) map[int64]*toolEvent {
	records := make(map[int64]*toolEvent, 4)
	for _, message := range messages {
		if message.Role == agent_model.MessageRoleTool {
			records[message.ID] = toolRecordOf(message.Content)
		}
	}
	return records
}

// attachmentPayloads renders the files of a message for the clients.
func attachmentPayloads(attachments []*agent_model.Attachment) []attachmentEvent {
	payloads := make([]attachmentEvent, 0, len(attachments))
	for _, attachment := range attachments {
		payloads = append(payloads, attachmentEvent{
			ID:        attachment.ID,
			Name:      attachment.Name,
			SizeText:  attachment.SizeText(),
			URL:       setting.AppSubURL + attachment.URL(),
			IsPicture: attachment.IsPicture(),
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
	message, _, err := agent_service.SubmitUserMessage(ctx, session, ctx.Doer.ID, ctx.FormString("content"))
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
	if _, err := agent_service.StartRun(ctx, session, ctx.Doer.ID); err != nil {
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

// ChatApprovalAllow lets a tool call the agent asked for go ahead.
func ChatApprovalAllow(ctx *context.Context) {
	chatApproval(ctx, true)
}

// ChatApprovalRefuse turns down a tool call the agent asked for.
func ChatApprovalRefuse(ctx *context.Context) {
	chatApproval(ctx, false)
}

// chatApproval answers one pending tool call and returns to the transcript.
func chatApproval(ctx *context.Context, allowed bool) {
	session, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}
	if err := agent_service.ResolveApproval(ctx, session.ID, ctx.PathParamInt64("aid"), ctx.Doer.ID, allowed); err != nil {
		switch {
		case errors.Is(err, agent_service.ErrApprovalNotPending{}):
			ctx.Flash.Error(ctx.Tr("agent.chat_approval_answered"))
		case errors.Is(err, agent_service.ErrApprovalNotOwner{}):
			ctx.Flash.Error(ctx.Tr("agent.chat_approval_not_yours"))
		default:
			ctx.ServerError("ResolveApproval", err)
			return
		}
	}
	ctx.Redirect(chatSessionURL(session.ID))
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
// the open session or the draft belongs to, else Pod.
func chatScope(ctx *context.Context, session *agent_model.Conversation, repo *repo_model.Repository) (string, string) {
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
	if repo != nil || (session != nil && session.Kind == agent_model.ConversationKindRepository) {
		return "repository", baseURL
	}
	return "pod", baseURL
}

// keepRepository narrows a list to the sessions of one repository, which is what the sidebar
// of that repository's pod shows.
func keepRepository(list *agent_service.ConversationList, repoID int64) *agent_service.ConversationList {
	kept := make([]*agent_service.RepositoryConversation, 0, len(list.Repositories))
	for _, entry := range list.Repositories {
		if entry.Repo.ID == repoID {
			kept = append(kept, entry)
		}
	}
	narrowed := *list
	narrowed.Repositories = kept
	return &narrowed
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
	for _, entry := range list.Repositories {
		if match(entry.Repo.FullName()) || match(entry.Session.Title) {
			filtered.Repositories = append(filtered.Repositories, entry)
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
	return len(list.Repositories)
}

// pendingState describes what the answer to the last message is doing, so a reloaded
// transcript shows the same thing the live one does.
func pendingState(run *agent_model.Run, labels map[string]string) (state, text string) {
	if run == nil {
		return "", ""
	}
	switch run.Status {
	case agent_model.RunStatusQueued:
		return "queued", labels["queued"]
	case agent_model.RunStatusRunning:
		return "running", labels["running"]
	case agent_model.RunStatusCanceled:
		return "canceled", labels["canceled"]
	case agent_model.RunStatusFailed:
		if run.Error == "" {
			return "failed", labels["failed"]
		}
		return "failed", labels["failed"] + ": " + run.Error
	}
	return "", ""
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

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	access_model "gitea.dev/models/perm/access"
	repo_model "gitea.dev/models/repo"
	unit_model "gitea.dev/models/unit"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/timeutil"
)

// ErrConversationNotFound is returned when a session does not exist.
type ErrConversationNotFound struct{}

func (ErrConversationNotFound) Error() string { return "agent session not found" }

// Is reports whether target is an ErrConversationNotFound.
func (ErrConversationNotFound) Is(target error) bool {
	_, ok := target.(ErrConversationNotFound)
	return ok
}

// ErrConversationDenied is returned when the doer may not use a session.
type ErrConversationDenied struct{}

func (ErrConversationDenied) Error() string { return "agent session is not accessible" }

// Is reports whether target is an ErrConversationDenied.
func (ErrConversationDenied) Is(target error) bool {
	_, ok := target.(ErrConversationDenied)
	return ok
}

// ErrResidentUndeletable is returned when someone tries to delete the instance-wide session.
type ErrResidentUndeletable struct{}

func (ErrResidentUndeletable) Error() string { return "the resident session can not be deleted" }

// Is reports whether target is an ErrResidentUndeletable.
func (ErrResidentUndeletable) Is(target error) bool {
	_, ok := target.(ErrResidentUndeletable)
	return ok
}

// RepositoryConversations groups the sessions of one repository.
type RepositoryConversations struct {
	Repo     *repo_model.Repository
	Sessions []*agent_model.Conversation
}

// FolderConversations groups the conversations filed under one folder.
type FolderConversations struct {
	Folder   *agent_model.ConversationFolder
	Sessions []*agent_model.Conversation
}

// ConversationList is the sidebar content: the resident session, the conversations (at the
// root and per folder) and the repository sessions grouped by repository.
type ConversationList struct {
	Resident     *agent_model.Conversation
	Servers      []*agent_model.Conversation
	Folders      []*FolderConversations
	Repositories []*RepositoryConversations
}

// ListConversations returns every session the doer may open. The resident session is
// created on first use so the instance always has one.
func ListConversations(ctx context.Context, doer *user_model.User) (*ConversationList, error) {
	list := &ConversationList{}
	var err error
	if doer.IsAdmin {
		if list.Resident, err = EnsureResidentConversation(ctx); err != nil {
			return nil, err
		}
		if list.Servers, err = findConversations(ctx, agent_model.ConversationKindServer, doer.ID); err != nil {
			return nil, err
		}
	}

	folders, err := ListConversationFolders(ctx)
	if err != nil {
		return nil, err
	}
	byFolder := make(map[int64]*FolderConversations, len(folders))
	for _, folder := range folders {
		group := &FolderConversations{Folder: folder}
		byFolder[folder.ID] = group
		list.Folders = append(list.Folders, group)
	}
	root := make([]*agent_model.Conversation, 0, len(list.Servers))
	for _, session := range list.Servers {
		if group := byFolder[session.FolderID]; group != nil {
			group.Sessions = append(group.Sessions, session)
			continue
		}
		root = append(root, session)
	}
	list.Servers = root

	sessions, err := findConversations(ctx, agent_model.ConversationKindRepository, doer.ID)
	if err != nil {
		return nil, err
	}
	repoIDs := make([]int64, 0, len(sessions))
	for _, session := range sessions {
		repoIDs = append(repoIDs, session.RepoID)
	}
	repos, err := repo_model.GetRepositoriesMapByIDs(ctx, repoIDs)
	if err != nil {
		return nil, err
	}
	byRepo := make(map[int64]*RepositoryConversations, len(list.Repositories))
	for _, session := range sessions {
		repo := repos[session.RepoID]
		if repo == nil {
			continue
		}
		allowed, err := CanUseRepository(ctx, doer, repo)
		if err != nil || !allowed {
			continue
		}
		group := byRepo[repo.ID]
		if group == nil {
			group = &RepositoryConversations{Repo: repo}
			byRepo[repo.ID] = group
			list.Repositories = append(list.Repositories, group)
		}
		group.Sessions = append(group.Sessions, session)
	}
	return list, nil
}

// findConversations lists the sessions of one owner; ownerID 0 is the instance-wide set.
func findConversations(ctx context.Context, kind string, ownerID int64) ([]*agent_model.Conversation, error) {
	sessions := make([]*agent_model.Conversation, 0, 8)
	err := db.GetEngine(ctx).
		Where("kind = ? AND owner_id = ?", kind, ownerID).
		OrderBy("updated_unix DESC").
		Find(&sessions)
	return sessions, err
}

// EnsureResidentConversation returns the instance-wide session, creating it when absent.
func EnsureResidentConversation(ctx context.Context) (*agent_model.Conversation, error) {
	sessions, err := findConversations(ctx, agent_model.ConversationKindResident, 0)
	if err != nil {
		return nil, err
	}
	if len(sessions) > 0 {
		return sessions[0], nil
	}
	session := &agent_model.Conversation{
		Kind: agent_model.ConversationKindResident,
		// the resident session keeps this name, so it is never renamed by the model
		Title:    "Pod",
		TitleSet: true,
	}
	if err := db.Insert(ctx, session); err != nil {
		return nil, fmt.Errorf("create resident session: %w", err)
	}
	return session, nil
}

// CreateConversation stores a new session of the given kind.
func CreateConversation(ctx context.Context, kind string, ownerID, repoID int64, title string) (*agent_model.Conversation, error) {
	if !agent_model.IsValidConversationKind(kind) {
		return nil, fmt.Errorf("unknown session kind %q", kind)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = defaultConversationTitle(kind)
	}
	session := &agent_model.Conversation{Kind: kind, OwnerID: ownerID, RepoID: repoID, Title: title}
	if err := db.Insert(ctx, session); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return session, nil
}

func defaultConversationTitle(kind string) string {
	switch kind {
	case agent_model.ConversationKindRepository:
		return "Repository agent"
	case agent_model.ConversationKindServer:
		return "Server agent"
	}
	return "Pod"
}

// GetConversation returns a session the doer may open.
func GetConversation(ctx context.Context, doer *user_model.User, id int64) (*agent_model.Conversation, error) {
	session, exist, err := db.GetByID[agent_model.Conversation](ctx, id)
	if err != nil {
		return nil, err
	}
	if !exist {
		return nil, ErrConversationNotFound{}
	}
	allowed, err := CanUseConversation(ctx, doer, session)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrConversationDenied{}
	}
	return session, nil
}

// CanUseConversation reports whether the doer may open the session: site admins own the
// resident and server sessions, repository sessions need write access to their repository.
func CanUseConversation(ctx context.Context, doer *user_model.User, session *agent_model.Conversation) (bool, error) {
	switch session.Kind {
	case agent_model.ConversationKindResident, agent_model.ConversationKindServer:
		return doer.IsAdmin, nil
	case agent_model.ConversationKindRepository:
		repo, err := repo_model.GetRepositoryByID(ctx, session.RepoID)
		if err != nil {
			return false, err
		}
		if repo == nil {
			return false, nil
		}
		return CanUseRepository(ctx, doer, repo)
	}
	return false, nil
}

// CanUseRepository reports whether the doer may drive an agent on the repository: the agent
// edits code, so code write access is the bar.
func CanUseRepository(ctx context.Context, doer *user_model.User, repo *repo_model.Repository) (bool, error) {
	perm, err := access_model.GetDoerRepoPermission(ctx, repo, doer)
	if err != nil {
		return false, err
	}
	return perm.CanWrite(unit_model.TypeCode), nil
}

// DeleteConversation removes a session and its transcript. The resident session stays.
func DeleteConversation(ctx context.Context, session *agent_model.Conversation) error {
	if session.Kind == agent_model.ConversationKindResident {
		return ErrResidentUndeletable{}
	}
	if _, err := db.GetEngine(ctx).Where("conversation_id = ?", session.ID).Delete(&agent_model.Message{}); err != nil {
		return err
	}
	_, err := db.DeleteByID[agent_model.Conversation](ctx, session.ID)
	return err
}

// ListMessages returns the newest messages of a session, oldest first. The transcript is
// paginated: the chat screen loads the tail and asks for older messages on demand.
func ListMessages(ctx context.Context, conversationID int64, limit int) ([]*agent_model.Message, error) {
	messages := make([]*agent_model.Message, 0, limit)
	query := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		OrderBy("id DESC").
		Limit(limit)
	if err := query.Find(&messages); err != nil {
		return nil, err
	}
	slices.Reverse(messages)
	return messages, nil
}

// ListMessagesBefore returns one page of older messages, oldest first.
func ListMessagesBefore(ctx context.Context, conversationID, beforeID int64, limit int) ([]*agent_model.Message, error) {
	messages := make([]*agent_model.Message, 0, limit)
	query := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		And("id < ?", beforeID).
		OrderBy("id DESC").
		Limit(limit)
	if err := query.Find(&messages); err != nil {
		return nil, err
	}
	slices.Reverse(messages)
	return messages, nil
}

// HasMessagesBefore reports whether the session has messages older than the given one.
func HasMessagesBefore(ctx context.Context, conversationID, beforeID int64) (bool, error) {
	if beforeID == 0 {
		return false, nil
	}
	return db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		And("id < ?", beforeID).
		Exist(&agent_model.Message{})
}

// ListAllMessages returns the whole transcript, oldest first; the export uses it.
func ListAllMessages(ctx context.Context, conversationID int64) ([]*agent_model.Message, error) {
	messages := make([]*agent_model.Message, 0, 64)
	err := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		OrderBy("id ASC").
		Find(&messages)
	return messages, err
}

// ConversationIcons are the icons an operator can give a session; empty means none.
func ConversationIcons() []string {
	return []string{
		"octicon-comment",
		"octicon-comment-discussion",
		"octicon-hash",
		"octicon-light-bulb",
		"octicon-rocket",
		"octicon-bug",
		"octicon-git-pull-request",
		"octicon-terminal",
		"octicon-book",
		"octicon-tools",
		"octicon-stack",
		"octicon-project",
		"octicon-server",
		"octicon-repo",
	}
}

// ErrConversationTitleInvalid is returned when a submitted title can not be used.
type ErrConversationTitleInvalid struct{}

func (ErrConversationTitleInvalid) Error() string { return "a conversation title is required" }

// Is reports whether target is an ErrConversationTitleInvalid.
func (ErrConversationTitleInvalid) Is(target error) bool {
	_, ok := target.(ErrConversationTitleInvalid)
	return ok
}

// ErrConversationImmutable is returned when a session keeps its name whatever happens.
type ErrConversationImmutable struct{}

func (ErrConversationImmutable) Error() string { return "the resident session keeps its name" }

// Is reports whether target is an ErrConversationImmutable.
func (ErrConversationImmutable) Is(target error) bool {
	_, ok := target.(ErrConversationImmutable)
	return ok
}

// SetConversationTitle renames a session. The resident session is not renamed: it is the
// instance-wide agent and is always called Pod.
func SetConversationTitle(ctx context.Context, conversation *agent_model.Conversation, title string) error {
	if conversation.Kind == agent_model.ConversationKindResident {
		return ErrConversationImmutable{}
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrConversationTitleInvalid{}
	}
	if utf8.RuneCountInString(title) > 100 {
		title = string([]rune(title)[:100])
	}
	conversation.Title = title
	conversation.TitleSet = true
	_, err := db.GetEngine(ctx).ID(conversation.ID).Cols("title", "title_set", "updated_unix").
		Update(&agent_model.Conversation{
			Title:       title,
			TitleSet:    true,
			UpdatedUnix: timeutil.TimeStampNow(),
		})
	return err
}

// SetConversationSummary stores what the session has been working on lately.
func SetConversationSummary(ctx context.Context, conversation *agent_model.Conversation, summary string) error {
	summary = strings.TrimSpace(summary)
	if runes := []rune(summary); len(runes) > 200 {
		summary = string(runes[:200])
	}
	conversation.Summary = summary
	_, err := db.GetEngine(ctx).ID(conversation.ID).Cols("summary", "updated_unix").
		Update(&agent_model.Conversation{Summary: summary, UpdatedUnix: timeutil.TimeStampNow()})
	return err
}

// SetConversationIcon sets the icon of a session; an empty name clears it.
func SetConversationIcon(ctx context.Context, conversation *agent_model.Conversation, icon string) error {
	if icon != "" && !slices.Contains(ConversationIcons(), icon) {
		return ErrConversationTitleInvalid{}
	}
	conversation.Icon = icon
	_, err := db.GetEngine(ctx).ID(conversation.ID).Cols("icon", "updated_unix").
		Update(&agent_model.Conversation{Icon: icon, UpdatedUnix: timeutil.TimeStampNow()})
	return err
}

// ErrConversationFolderInvalid is returned when a folder can not be stored as requested.
type ErrConversationFolderInvalid struct{}

func (ErrConversationFolderInvalid) Error() string { return "a folder name is required" }

// Is reports whether target is an ErrConversationFolderInvalid.
func (ErrConversationFolderInvalid) Is(target error) bool {
	_, ok := target.(ErrConversationFolderInvalid)
	return ok
}

// ErrConversationFolderNotFound is returned when the folder does not exist.
type ErrConversationFolderNotFound struct{}

func (ErrConversationFolderNotFound) Error() string { return "conversation folder not found" }

// Is reports whether target is an ErrConversationFolderNotFound.
func (ErrConversationFolderNotFound) Is(target error) bool {
	_, ok := target.(ErrConversationFolderNotFound)
	return ok
}

// ConversationFolderIcons are the icons a folder can use; empty means none.
func ConversationFolderIcons() []string {
	return ConversationIcons()
}

// ListConversationFolders returns the folders in display order.
func ListConversationFolders(ctx context.Context) ([]*agent_model.ConversationFolder, error) {
	folders := make([]*agent_model.ConversationFolder, 0, 8)
	err := db.GetEngine(ctx).OrderBy("position ASC, name ASC").Find(&folders)
	return folders, err
}

// GetConversationFolder returns one folder.
func GetConversationFolder(ctx context.Context, id int64) (*agent_model.ConversationFolder, error) {
	folder, exist, err := db.GetByID[agent_model.ConversationFolder](ctx, id)
	if err != nil {
		return nil, err
	}
	if !exist {
		return nil, ErrConversationFolderNotFound{}
	}
	return folder, nil
}

// CreateConversationFolder stores a folder.
func CreateConversationFolder(ctx context.Context, name, icon string) (*agent_model.ConversationFolder, error) {
	name, icon, err := normalizeFolder(name, icon)
	if err != nil {
		return nil, err
	}
	folder := &agent_model.ConversationFolder{Name: name, Icon: icon}
	if err := db.Insert(ctx, folder); err != nil {
		return nil, fmt.Errorf("insert folder: %w", err)
	}
	return folder, nil
}

// UpdateConversationFolder renames a folder and sets its icon.
func UpdateConversationFolder(ctx context.Context, id int64, name, icon string) (*agent_model.ConversationFolder, error) {
	folder, err := GetConversationFolder(ctx, id)
	if err != nil {
		return nil, err
	}
	name, icon, err = normalizeFolder(name, icon)
	if err != nil {
		return nil, err
	}
	folder.Name, folder.Icon = name, icon
	if _, err := db.GetEngine(ctx).ID(id).Cols("name", "icon", "updated_unix").
		Update(&agent_model.ConversationFolder{Name: name, Icon: icon, UpdatedUnix: timeutil.TimeStampNow()}); err != nil {
		return nil, err
	}
	return folder, nil
}

func normalizeFolder(name, icon string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", ErrConversationFolderInvalid{}
	}
	if utf8.RuneCountInString(name) > 100 {
		name = string([]rune(name)[:100])
	}
	if icon != "" && !slices.Contains(ConversationFolderIcons(), icon) {
		return "", "", ErrConversationFolderInvalid{}
	}
	return name, icon, nil
}

// DeleteConversationFolder removes a folder; its conversations move back to the root.
func DeleteConversationFolder(ctx context.Context, id int64) error {
	if _, err := GetConversationFolder(ctx, id); err != nil {
		return err
	}
	if _, err := db.GetEngine(ctx).Where("folder_id = ?", id).Cols("folder_id").
		Update(&agent_model.Conversation{FolderID: 0}); err != nil {
		return err
	}
	_, err := db.DeleteByID[agent_model.ConversationFolder](ctx, id)
	return err
}

// SetConversationFolder files a conversation under a folder; 0 moves it back to the root.
func SetConversationFolder(ctx context.Context, conversation *agent_model.Conversation, folderID int64) error {
	if folderID != 0 {
		if _, err := GetConversationFolder(ctx, folderID); err != nil {
			return err
		}
	}
	conversation.FolderID = folderID
	_, err := db.GetEngine(ctx).ID(conversation.ID).Cols("folder_id", "updated_unix").
		Update(&agent_model.Conversation{FolderID: folderID, UpdatedUnix: timeutil.TimeStampNow()})
	return err
}

// ErrMessageNotEditable is returned when a message can not be edited.
type ErrMessageNotEditable struct{}

func (ErrMessageNotEditable) Error() string { return "only a user message can be edited" }

// Is reports whether target is an ErrMessageNotEditable.
func (ErrMessageNotEditable) Is(target error) bool {
	_, ok := target.(ErrMessageNotEditable)
	return ok
}

// EditUserMessage rewrites a user message and drops everything after it, so the agent
// answers the edited message again.
func EditUserMessage(ctx context.Context, conversation *agent_model.Conversation, messageID int64, content string) (*agent_model.Message, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("a message needs content")
	}
	message, exist, err := db.GetByID[agent_model.Message](ctx, messageID)
	if err != nil {
		return nil, err
	}
	if !exist || message.ConversationID != conversation.ID {
		return nil, ErrConversationNotFound{}
	}
	if message.Role != agent_model.MessageRoleUser {
		return nil, ErrMessageNotEditable{}
	}
	if err := DeleteMessagesAfter(ctx, conversation.ID, messageID); err != nil {
		return nil, err
	}
	// the edit rewinds the conversation to this message: the run that started after it
	// re-answers from here, which is why the transcript before it is left untouched
	message.Content = content
	message.EditedUnix = timeutil.TimeStampNow()
	if _, err := db.GetEngine(ctx).ID(message.ID).Cols("content", "edited_unix").Update(message); err != nil {
		return nil, err
	}
	return message, nil
}

// DeleteMessagesAfter removes every message newer than the given one, together with the
// files those messages carried.
func DeleteMessagesAfter(ctx context.Context, conversationID, messageID int64) error {
	attachments := make([]*agent_model.Attachment, 0, 4)
	if err := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		And("message_id > ?", messageID).
		Find(&attachments); err != nil {
		return err
	}
	for _, attachment := range attachments {
		if err := os.Remove(AttachmentPath(attachment)); err != nil && !os.IsNotExist(err) {
			log.Warn("unable to remove attachment file %d: %v", attachment.ID, err)
		}
	}
	if _, err := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		And("message_id > ?", messageID).
		Delete(&agent_model.Attachment{}); err != nil {
		return err
	}
	_, err := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		And("id > ?", messageID).
		Delete(&agent_model.Message{})
	return err
}

// GetMessage returns one transcript entry.
func GetMessage(ctx context.Context, id int64) (*agent_model.Message, error) {
	message, exist, err := db.GetByID[agent_model.Message](ctx, id)
	if err != nil || !exist {
		return nil, err
	}
	return message, nil
}

// AppendMessage stores one transcript entry and marks the session as recently used.
func AppendMessage(ctx context.Context, conversationID int64, role, content string) (*agent_model.Message, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("a message needs content")
	}
	switch role {
	// every caller passes a literal role, so a reader can never forge one of these
	case agent_model.MessageRoleUser, agent_model.MessageRoleAssistant, agent_model.MessageRoleSystem, agent_model.MessageRoleTool:
	default:
		return nil, fmt.Errorf("unknown message role %q", role)
	}
	message := &agent_model.Message{ConversationID: conversationID, Role: role, Content: content}
	if err := db.Insert(ctx, message); err != nil {
		return nil, err
	}
	_, err := db.GetEngine(ctx).
		ID(conversationID).
		Cols("updated_unix").
		Update(&agent_model.Conversation{UpdatedUnix: timeutil.TimeStampNow()})
	if err != nil {
		return nil, err
	}
	return message, nil
}

// WritableRepositories lists recent repositories the doer may drive an agent on, for the
// repository picker of the chat screen.
func WritableRepositories(ctx context.Context, doer *user_model.User, limit int) ([]*repo_model.Repository, error) {
	repos, _, err := repo_model.SearchRepository(ctx, repo_model.SearchRepoOptions{
		ListOptions: db.ListOptions{Page: 1, PageSize: limit},
		Actor:       doer,
		Private:     true,
		OrderBy:     db.SearchOrderByRecentUpdated,
	})
	if err != nil {
		return nil, err
	}
	writable := make([]*repo_model.Repository, 0, len(repos))
	for _, repo := range repos {
		allowed, err := CanUseRepository(ctx, doer, repo)
		if err != nil {
			return nil, err
		}
		if allowed {
			writable = append(writable, repo)
		}
	}
	return writable, nil
}

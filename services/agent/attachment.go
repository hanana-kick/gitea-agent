// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/setting"
)

// ErrAttachmentInvalid is returned when a file can not be accepted.
type ErrAttachmentInvalid struct {
	Reason string
}

func (err ErrAttachmentInvalid) Error() string { return err.Reason }

// Is reports whether target is an ErrAttachmentInvalid.
func (err ErrAttachmentInvalid) Is(target error) bool {
	_, ok := target.(ErrAttachmentInvalid)
	return ok
}

// ErrAttachmentNotFound is returned when the file does not exist.
type ErrAttachmentNotFound struct{}

func (ErrAttachmentNotFound) Error() string { return "attachment not found" }

// Is reports whether target is an ErrAttachmentNotFound.
func (err ErrAttachmentNotFound) Is(target error) bool {
	_, ok := target.(ErrAttachmentNotFound)
	return ok
}

// AttachmentAllowed reports whether the instance accepts uploads at all.
func AttachmentAllowed() bool {
	return setting.Attachment.Enabled
}

// MaxAttachmentBytes is the size limit of one uploaded file.
func MaxAttachmentBytes() int64 {
	return setting.Attachment.MaxSize * 1024 * 1024
}

// attachmentImageLimit caps what is handed to a model as a picture; a bigger file stays
// a download for the reader.
const attachmentImageLimit = 4 * 1024 * 1024

// ImageDataURL returns a picture as a data URL the model can be shown, or "" when the
// file is not a picture, is too big to send, or cannot be read.
func ImageDataURL(attachment *agent_model.Attachment) string {
	kind := agent_model.PictureMime(attachment.Name)
	if kind == "" || attachment.Size > attachmentImageLimit {
		return ""
	}
	file, err := os.Open(AttachmentPath(attachment))
	if err != nil {
		return ""
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, attachmentImageLimit))
	if err != nil || len(content) == 0 {
		return ""
	}
	return "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(content)
}

// StoreAttachment writes one uploaded file and records it against a message.
func StoreAttachment(ctx context.Context, conversationID, messageID int64, name string, content io.Reader, size int64) (*agent_model.Attachment, error) {
	if !AttachmentAllowed() {
		return nil, ErrAttachmentInvalid{Reason: "attachments are disabled"}
	}
	if limit := MaxAttachmentBytes(); size > limit {
		return nil, ErrAttachmentInvalid{Reason: fmt.Sprintf("the file is larger than %d MB", setting.Attachment.MaxSize)}
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." {
		return nil, ErrAttachmentInvalid{Reason: "the file needs a name"}
	}
	if ext := strings.ToLower(filepath.Ext(name)); ext != "" && !AllowedAttachmentType(ext) {
		return nil, ErrAttachmentInvalid{Reason: "this file type is not allowed"}
	}

	attachment := &agent_model.Attachment{ConversationID: conversationID, MessageID: messageID, Name: name, Size: size}
	if err := db.Insert(ctx, attachment); err != nil {
		return nil, fmt.Errorf("insert attachment: %w", err)
	}

	path := AttachmentPath(attachment)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create attachment directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return nil, fmt.Errorf("create attachment: %w", err)
	}
	defer file.Close()
	written, err := io.Copy(file, content)
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("write attachment: %w", err)
	}
	attachment.Size = written
	if _, err := db.GetEngine(ctx).ID(attachment.ID).Cols("size").Update(attachment); err != nil {
		return nil, err
	}
	return attachment, nil
}

// AllowedAttachmentType reports whether the extension is one the instance accepts.
func AllowedAttachmentType(ext string) bool {
	for allowed := range strings.SplitSeq(setting.Attachment.AllowedTypes, ",") {
		if allowed != "" && strings.EqualFold(strings.TrimSpace(allowed), ext) {
			return true
		}
	}
	return false
}

// AttachmentPath returns where the bytes of an attachment are stored.
func AttachmentPath(attachment *agent_model.Attachment) string {
	return filepath.Join(setting.AppDataPath, "agent-attachments", strconv.FormatInt(attachment.ConversationID, 10), strconv.FormatInt(attachment.ID, 10))
}

// GetAttachment returns one attachment of a conversation.
func GetAttachment(ctx context.Context, conversationID, id int64) (*agent_model.Attachment, error) {
	attachment := &agent_model.Attachment{}
	exist, err := db.GetEngine(ctx).
		Where("id = ?", id).
		And("conversation_id = ?", conversationID).
		Get(attachment)
	if err != nil {
		return nil, err
	}
	if !exist {
		return nil, ErrAttachmentNotFound{}
	}
	return attachment, nil
}

// ListMessageAttachments returns the files of the given messages, keyed by message.
func ListMessageAttachments(ctx context.Context, messageIDs []int64) (map[int64][]*agent_model.Attachment, error) {
	byMessage := make(map[int64][]*agent_model.Attachment, len(messageIDs))
	if len(messageIDs) == 0 {
		return byMessage, nil
	}
	attachments := make([]*agent_model.Attachment, 0, len(messageIDs))
	if err := db.GetEngine(ctx).In("message_id", messageIDs).OrderBy("id ASC").Find(&attachments); err != nil {
		return nil, err
	}
	for _, attachment := range attachments {
		byMessage[attachment.MessageID] = append(byMessage[attachment.MessageID], attachment)
	}
	return byMessage, nil
}

// DeleteConversationAttachments removes the files of a conversation.
func DeleteConversationAttachments(ctx context.Context, conversationID int64) error {
	if _, err := db.GetEngine(ctx).Where("conversation_id = ?", conversationID).Delete(&agent_model.Attachment{}); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(setting.AppDataPath, "agent-attachments", strconv.FormatInt(conversationID, 10)))
}

// attachmentTextLimit caps how much of a text file is handed to the model.
const attachmentTextLimit = 64 * 1024

// AttachmentText returns the content of a text attachment for the model, or "" when the
// file is binary or too large to be worth reading.
func AttachmentText(attachment *agent_model.Attachment) string {
	if attachment.Size > attachmentTextLimit {
		return ""
	}
	content, err := os.ReadFile(AttachmentPath(attachment))
	if err != nil {
		return ""
	}
	// a NUL byte or invalid utf-8 means it is not text
	if !utf8.Valid(content) || bytes.ContainsRune(content, 0) {
		return ""
	}
	return string(content)
}

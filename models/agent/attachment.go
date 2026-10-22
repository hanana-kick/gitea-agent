// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// Attachment is a file sent with a message. The bytes stay on disk under the instance data
// directory and are only served to a user who may open the conversation.
type Attachment struct {
	ID             int64  `xorm:"pk autoincr"`
	ConversationID int64  `xorm:"INDEX NOT NULL"`
	MessageID      int64  `xorm:"INDEX NOT NULL"`
	Name           string `xorm:"VARCHAR(255) NOT NULL"`
	Size           int64  `xorm:"NOT NULL DEFAULT 0"`

	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
}

// SizeText renders the size the way the transcript shows it.
func (a *Attachment) SizeText() string {
	const unit = 1024
	if a.Size < unit {
		return fmt.Sprintf("%d B", a.Size)
	}
	value := float64(a.Size)
	for _, suffix := range []string{"KB", "MB", "GB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TB", value/unit)
}

// URL is the download path of the attachment.
func (a *Attachment) URL() string {
	return "/-/agent/" + strconv.FormatInt(a.ConversationID, 10) + "/attachments/" + strconv.FormatInt(a.ID, 10)
}

// PictureMime returns the media type of a picture this instance shows inline, or "" when the
// file is not one of them. Everything that decides what to do with a picture reads this.
func PictureMime(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// IsPicture reports whether the transcript can show the file itself instead of its name.
func (a *Attachment) IsPicture() bool {
	return PictureMime(a.Name) != ""
}

func (Attachment) TableName() string {
	return "agent_attachment"
}

func init() {
	db.RegisterModel(new(Attachment))
}

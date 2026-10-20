// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// APIKey is a named LLM credential. The value is encrypted with setting.SecretKey and is
// only decrypted while a request is being made: it is never handed back to a client.
type APIKey struct {
	ID      int64  `xorm:"pk autoincr"`
	Name    string `xorm:"UNIQUE NOT NULL"`
	OwnerID int64  `xorm:"INDEX NOT NULL"` // who created it
	// SecretEnc holds the encrypted key.
	SecretEnc string `xorm:"LONGTEXT NOT NULL"`

	CreatedUnix  timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix  timeutil.TimeStamp `xorm:"updated NOT NULL"`
	LastUsedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (APIKey) TableName() string {
	return "agent_api_key"
}

func init() {
	db.RegisterModel(new(APIKey))
}

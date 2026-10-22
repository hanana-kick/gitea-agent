// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"
	"fmt"

	"gitea.dev/modelmigration/base"
	"gitea.dev/modules/timeutil"

	"xorm.io/xorm"
)

// agentAPIKeyV361 is the frozen shape of agent_api_key, a named LLM credential.
type agentAPIKeyV361 struct {
	ID      int64  `xorm:"pk autoincr"`
	Name    string `xorm:"UNIQUE NOT NULL"`
	OwnerID int64  `xorm:"INDEX NOT NULL"`
	// SecretEnc holds the encrypted key.
	SecretEnc string `xorm:"LONGTEXT NOT NULL"`

	CreatedUnix  timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix  timeutil.TimeStamp `xorm:"updated NOT NULL"`
	LastUsedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

func (agentAPIKeyV361) TableName() string {
	return "agent_api_key"
}

// agentProviderV361 is the frozen shape of agent_provider at the point its credentials
// move into agent_api_key.
type agentProviderV361 struct {
	ID         int64  `xorm:"pk autoincr"`
	UserID     int64  `xorm:"INDEX NOT NULL"`
	Name       string `xorm:"NOT NULL"`
	Provider   string `xorm:"NOT NULL"`
	BaseURL    string
	Model      string `xorm:"NOT NULL"`
	ModelsJSON string `xorm:"TEXT"`

	APIKeyID    int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
	HeadersJSON string             `xorm:"TEXT"`
	HasKey      bool               `xorm:"-"`
	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated NOT NULL"`
}

func (agentProviderV361) TableName() string {
	return "agent_provider"
}

// AddAgentAPIKeyTable moves provider credentials into their own managed table. Stored keys
// are copied as-is: they are already encrypted with the instance secret key.
func AddAgentAPIKeyTable(_ context.Context, x base.EngineMigration) error {
	if _, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agentAPIKeyV361), new(agentProviderV361)); err != nil {
		return err
	}

	type storedKey struct {
		ID        int64  `xorm:"id"`
		Name      string `xorm:"name"`
		UserID    int64  `xorm:"user_id"`
		APIKeyEnc string `xorm:"api_key_enc"`
	}
	keys := make([]storedKey, 0, 4)
	if err := x.SQL("SELECT `id`, `name`, `user_id`, `api_key_enc` FROM `agent_provider` WHERE `api_key_enc` <> ''").Find(&keys); err != nil {
		return fmt.Errorf("read stored provider keys: %w", err)
	}
	for _, stored := range keys {
		key := &agentAPIKeyV361{
			Name:      stored.Name,
			OwnerID:   stored.UserID,
			SecretEnc: stored.APIKeyEnc,
		}
		if _, err := x.Insert(key); err != nil {
			return fmt.Errorf("move key of provider %d: %w", stored.ID, err)
		}
		if _, err := x.Exec("UPDATE `agent_provider` SET `api_key_id` = ? WHERE `id` = ?", key.ID, stored.ID); err != nil {
			return fmt.Errorf("bind key to provider %d: %w", stored.ID, err)
		}
	}

	// drop the columns now that the keys live in agent_api_key
	return base.RecreateTables(new(agentProviderV361))(x)
}

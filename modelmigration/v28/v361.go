// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v28

import (
	"context"
	"fmt"

	"gitea.dev/modelmigration/base"
	agent_model "gitea.dev/models/agent"

	"xorm.io/xorm"
)

// AddAgentAPIKeyTable moves provider credentials into their own managed table. Stored keys
// are copied as-is: they are already encrypted with the instance secret key.
func AddAgentAPIKeyTable(_ context.Context, x base.EngineMigration) error {
	if _, err := x.SyncWithOptions(xorm.SyncOptions{
		IgnoreConstrains:  true,
		IgnoreDropIndices: true,
	}, new(agent_model.APIKey), new(agent_model.Provider)); err != nil {
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
		key := &agent_model.APIKey{
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
	return base.RecreateTables(new(agent_model.Provider))(x)
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/log"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
)

// ErrAPIKeyInvalid is returned when a key can not be stored as requested.
type ErrAPIKeyInvalid struct {
	Reason string
}

func (err ErrAPIKeyInvalid) Error() string { return err.Reason }

// Is reports whether target is an ErrAPIKeyInvalid.
func (err ErrAPIKeyInvalid) Is(target error) bool {
	_, ok := target.(ErrAPIKeyInvalid)
	return ok
}

// ErrAPIKeyNotFound is returned when the key does not exist.
type ErrAPIKeyNotFound struct{}

func (ErrAPIKeyNotFound) Error() string { return "api key not found" }

// Is reports whether target is an ErrAPIKeyNotFound.
func (ErrAPIKeyNotFound) Is(target error) bool {
	_, ok := target.(ErrAPIKeyNotFound)
	return ok
}

// ErrAPIKeyInUse is returned when a key still backs a provider.
type ErrAPIKeyInUse struct{}

func (ErrAPIKeyInUse) Error() string { return "the api key is still used by a provider" }

// Is reports whether target is an ErrAPIKeyInUse.
func (ErrAPIKeyInUse) Is(target error) bool {
	_, ok := target.(ErrAPIKeyInUse)
	return ok
}

// CreateAPIKey stores a new credential. The value is encrypted right away and is never
// readable again, not even by an administrator.
func CreateAPIKey(ctx context.Context, ownerID int64, name, value string) (*agent_model.APIKey, error) {
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" {
		return nil, ErrAPIKeyInvalid{Reason: "a key needs a name"}
	}
	if value == "" {
		return nil, ErrAPIKeyInvalid{Reason: "a key needs a value"}
	}
	existing, err := apiKeyByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrAPIKeyInvalid{Reason: fmt.Sprintf("key %q already exists", name)}
	}
	encrypted, err := secret_module.EncryptSecret(setting.SecretKey, value)
	if err != nil {
		return nil, fmt.Errorf("encrypt api key: %w", err)
	}
	key := &agent_model.APIKey{Name: name, OwnerID: ownerID, SecretEnc: encrypted}
	if err := db.Insert(ctx, key); err != nil {
		return nil, fmt.Errorf("insert api key: %w", err)
	}
	return key, nil
}

// ListAPIKeys returns every stored key, newest first. The values are not included.
func ListAPIKeys(ctx context.Context) ([]*agent_model.APIKey, error) {
	keys := make([]*agent_model.APIKey, 0, 8)
	err := db.GetEngine(ctx).OrderBy("name ASC").Find(&keys)
	return keys, err
}

// GetAPIKey returns one key without its value.
func GetAPIKey(ctx context.Context, id int64) (*agent_model.APIKey, error) {
	key, exist, err := db.GetByID[agent_model.APIKey](ctx, id)
	if err != nil {
		return nil, err
	}
	if !exist {
		return nil, ErrAPIKeyNotFound{}
	}
	return key, nil
}

func apiKeyByName(ctx context.Context, name string) (*agent_model.APIKey, error) {
	key := &agent_model.APIKey{}
	exist, err := db.GetEngine(ctx).Where("name = ?", name).Get(key)
	if err != nil || !exist {
		return nil, err
	}
	return key, nil
}

// ReplaceAPIKeyValue swaps the secret of an existing key, keeping its name and bindings.
func ReplaceAPIKeyValue(ctx context.Context, id int64, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return ErrAPIKeyInvalid{Reason: "a key needs a value"}
	}
	if _, err := GetAPIKey(ctx, id); err != nil {
		return err
	}
	encrypted, err := secret_module.EncryptSecret(setting.SecretKey, value)
	if err != nil {
		return fmt.Errorf("encrypt api key: %w", err)
	}
	_, err = db.GetEngine(ctx).ID(id).Cols("secret_enc", "updated_unix").
		Update(&agent_model.APIKey{SecretEnc: encrypted, UpdatedUnix: timeutil.TimeStampNow()})
	return err
}

// DeleteAPIKey removes a key that no provider uses.
func DeleteAPIKey(ctx context.Context, id int64) error {
	if _, err := GetAPIKey(ctx, id); err != nil {
		return err
	}
	inUse, err := APIKeyInUse(ctx, id)
	if err != nil {
		return err
	}
	if inUse {
		return ErrAPIKeyInUse{}
	}
	_, err = db.DeleteByID[agent_model.APIKey](ctx, id)
	return err
}

// APIKeyInUse reports whether a provider is bound to the key.
func APIKeyInUse(ctx context.Context, id int64) (bool, error) {
	return db.GetEngine(ctx).Where("api_key_id = ?", id).Exist(&agent_model.Provider{})
}

// APIKeyUsage maps a key ID to the providers bound to it. Deleting a key needs this to
// explain what still depends on it.
func APIKeyUsage(ctx context.Context) (map[int64][]string, error) {
	providers, err := GetProviders(ctx, "")
	if err != nil {
		return nil, err
	}
	usage := make(map[int64][]string)
	for _, provider := range providers {
		if provider.APIKeyID == 0 {
			continue
		}
		usage[provider.APIKeyID] = append(usage[provider.APIKeyID], provider.Name)
	}
	return usage, nil
}

// APIKeyNames maps key IDs to their names, for showing what a provider is bound to.
func APIKeyNames(ctx context.Context) (map[int64]string, error) {
	keys, err := ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(keys))
	for _, key := range keys {
		names[key.ID] = key.Name
	}
	return names, nil
}

// APIKeyValue decrypts a stored key. It is only called while a request is being made.
func APIKeyValue(ctx context.Context, key *agent_model.APIKey) string {
	if key == nil || key.SecretEnc == "" {
		return ""
	}
	value, err := secret_module.DecryptSecret(setting.SecretKey, key.SecretEnc)
	if err != nil {
		log.Warn("failed to decrypt api key %d: %v", key.ID, err)
		return ""
	}
	return value
}

// ResolveProviderAPIKey returns the value a provider authenticates with, decrypting it on
// the spot and recording the use.
func ResolveProviderAPIKey(ctx context.Context, p *agent_model.Provider) string {
	if p == nil || p.APIKeyID == 0 {
		return ""
	}
	key, err := GetAPIKey(ctx, p.APIKeyID)
	if err != nil {
		if !errors.Is(err, ErrAPIKeyNotFound{}) {
			log.Error("unable to load api key %d: %v", p.APIKeyID, err)
		}
		return ""
	}
	value := APIKeyValue(ctx, key)
	if value == "" {
		return ""
	}
	if _, err := db.GetEngine(ctx).ID(key.ID).Cols("last_used_unix").
		Update(&agent_model.APIKey{LastUsedUnix: timeutil.TimeStampNow()}); err != nil {
		log.Warn("unable to record api key %d use: %v", key.ID, err)
	}
	return value
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyValueDecryptsStoredSecret(t *testing.T) {
	encrypted, err := secret_module.EncryptSecret(setting.SecretKey, "sk-stored")
	require.NoError(t, err)
	encryptedWrongKey, err := secret_module.EncryptSecret("another-key", "sk-stored")
	require.NoError(t, err)

	assert.Equal(t, "sk-stored", APIKeyValue(context.Background(), &agent_model.APIKey{SecretEnc: encrypted}))
	// a key encrypted with a different secret can not be recovered
	assert.Empty(t, APIKeyValue(context.Background(), &agent_model.APIKey{SecretEnc: encryptedWrongKey}))
	// a provider without a bound key resolves to nothing
	assert.Empty(t, ResolveProviderAPIKey(context.Background(), &agent_model.Provider{}))
}

func TestAPIKeyRejectsEmptyFields(t *testing.T) {
	_, err := CreateAPIKey(context.Background(), 1, "", "sk")
	assert.ErrorIs(t, err, ErrAPIKeyInvalid{})

	_, err = CreateAPIKey(context.Background(), 1, "name", "   ")
	assert.ErrorIs(t, err, ErrAPIKeyInvalid{})

	assert.ErrorIs(t, ReplaceAPIKeyValue(context.Background(), 1, ""), ErrAPIKeyInvalid{})
}

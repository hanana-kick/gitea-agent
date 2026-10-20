// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	secret_module "gitea.dev/modules/secret"
	"gitea.dev/modules/setting"
	llm "gitea.dev/services/agent/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupVariableFallsBackToEnvironment(t *testing.T) {
	// Actions disabled keeps this test DB-free; the process environment is consulted instead.
	setting.Actions.Enabled = false
	t.Cleanup(func() { setting.Actions.Enabled = true })

	t.Setenv("AGENT_TEST_ENV_KEY", "from-env")
	assert.Equal(t, "from-env", lookupVariable(context.Background(), "AGENT_TEST_ENV_KEY"))
	assert.Empty(t, lookupVariable(context.Background(), "AGENT_TEST_MISSING_KEY"))
}

func TestResolveAPIKeyUsesStoredKey(t *testing.T) {
	encrypted, err := secret_module.EncryptSecret(setting.SecretKey, "sk-stored")
	require.NoError(t, err)
	encryptedWrongKey, err := secret_module.EncryptSecret("another-key", "sk-stored")
	require.NoError(t, err)

	assert.Equal(t, "sk-stored", resolveAPIKey(context.Background(), &agent_model.Provider{APIKeyEnc: encrypted}))
	// a key encrypted with a different secret can not be recovered
	assert.Empty(t, resolveAPIKey(context.Background(), &agent_model.Provider{APIKeyEnc: encryptedWrongKey}))
	// no key at all
	assert.Empty(t, resolveAPIKey(context.Background(), &agent_model.Provider{}))
}

func TestLookupVariablePrefersActionsValue(t *testing.T) {
	original := actionsValueAt
	t.Cleanup(func() { actionsValueAt = original })
	t.Setenv("AGENT_TEST_ENV_KEY", "from-env")

	actionsValueAt = func(_ context.Context, name string) (string, bool) {
		if name == "AGENT_TEST_ACTIONS_KEY" {
			return "from-actions", true
		}
		return "", false
	}

	assert.Equal(t, "from-actions", lookupVariable(context.Background(), "AGENT_TEST_ACTIONS_KEY"))
	assert.Equal(t, "from-env", lookupVariable(context.Background(), "AGENT_TEST_ENV_KEY"))
	assert.Empty(t, lookupVariable(context.Background(), "AGENT_TEST_MISSING_KEY"))
}

func TestResolveAPIKeyPrefersActionsValue(t *testing.T) {
	original := actionsValueAt
	t.Cleanup(func() { actionsValueAt = original })
	actionsValueAt = func(_ context.Context, name string) (string, bool) {
		return "from-actions", name == "AGENT_TEST_ACTIONS_KEY"
	}

	encrypted, err := secret_module.EncryptSecret(setting.SecretKey, "sk-stored")
	require.NoError(t, err)

	fromActions := resolveAPIKey(context.Background(), &agent_model.Provider{APIKeyEnc: encrypted, EnvKey: "AGENT_TEST_ACTIONS_KEY"})
	assert.Equal(t, "from-actions", fromActions)

	// an unknown variable name falls back to the stored key
	fromStored := resolveAPIKey(context.Background(), &agent_model.Provider{APIKeyEnc: encrypted, EnvKey: "AGENT_TEST_MISSING"})
	assert.Equal(t, "sk-stored", fromStored)
}

func TestResolveKeySource(t *testing.T) {
	cases := []struct {
		name string
		opts ProviderOptions
		want string
	}{
		{"explicit source wins", ProviderOptions{KeySource: KeySourceEnvKey, APIKey: "sk"}, KeySourceEnvKey},
		{"env key only", ProviderOptions{EnvKey: "MY_KEY"}, KeySourceEnvKey},
		{"api key only", ProviderOptions{APIKey: "sk"}, KeySourceAPIKey},
		{"nothing mentioned keeps the configuration", ProviderOptions{}, KeySourceKeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.opts.resolveKeySource())
		})
	}

	// a create without any key information is rejected
	_, err := CreateProvider(context.Background(), 1, ProviderOptions{Name: "x", Provider: llm.ProviderOpenRouter, Models: []agent_model.ProviderModel{{ID: "m"}}})
	assert.ErrorIs(t, err, ErrNoKeySource{})
}

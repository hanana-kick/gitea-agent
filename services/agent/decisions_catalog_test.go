// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A gateway that fronts several kinds of model lists the ones that answer decisions apart from the
// chat ones. A model offered to a decision role is one of them, so the fetch asks for both listings
// and keeps which is which.
func TestFetchModelsKeepsWhatServesDecisions(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	asked := make([]string, 0, 4)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("endpoints") != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "parallel/decisions-fast:free", "name": "Parallel: Decisions Fast (free)"},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "acme/chat-one", "name": "Acme: Chat One"},
			{"id": "parallel/decisions-fast:free", "name": "Parallel: Decisions Fast (free)"},
		}})
	}))
	defer gateway.Close()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	key, err := CreateAPIKey(ctx, owner.ID, "fetch-key", "sk-fetch")
	require.NoError(t, err)
	provider := &agent_model.Provider{UserID: owner.ID, Name: "gateway", Provider: "openrouter", BaseURL: gateway.URL, Model: "acme/chat-one", APIKeyID: key.ID}
	require.NoError(t, db.Insert(ctx, provider))
	t.Cleanup(func() {
		_, _ = db.DeleteByID[agent_model.Provider](context.Background(), provider.ID)
	})

	models, err := FetchModels(ctx, ModelFetchOptions{Provider: "openrouter", BaseURL: gateway.URL, APIKeyID: key.ID})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(asked), 2, "the decisions listing was not asked for")

	byID := map[string]agent_model.ProviderModel{}
	for _, m := range models {
		byID[m.ID] = m
	}
	assert.False(t, byID["acme/chat-one"].Decisions, "a chat model answers decisions?")
	assert.True(t, byID["parallel/decisions-fast:free"].Decisions, "the model the decisions listing reported is not marked")

	// once the form saves what it was shown, it reaches the pickers a decision role is chosen from
	require.NoError(t, provider.SetModels(models))
	_, err = db.GetEngine(ctx).ID(provider.ID).Cols("models_json").Update(provider)
	require.NoError(t, err)

	options, err := ModelOptions(ctx, 200)
	require.NoError(t, err)
	ref := ModelRefForProvider(provider.ID, "parallel/decisions-fast:free")
	found := false
	for _, option := range options {
		if option.Ref == ref {
			found = true
			assert.True(t, option.Decisions, "the picker does not say what the model serves")
		}
	}
	assert.True(t, found, "the decisions model is not offered")
}

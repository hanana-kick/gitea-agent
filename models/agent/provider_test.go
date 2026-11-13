// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A row stored before models said what they serve still reads as a model that answers a chat.
func TestProviderModelsReadWhatAModelServes(t *testing.T) {
	p := &Provider{}
	p.ModelsJSON = `[{"id":"acme/legacy","name":"Acme: Legacy"},{"id":"only/decides","decisions":true}]`
	models := p.Models()
	require.Len(t, models, 2)
	assert.True(t, models[0].Chat, "a model stored earlier lost its place in the lists")
	assert.False(t, models[0].Decisions)
	assert.True(t, models[1].Decisions)
	assert.False(t, models[1].Chat, "a decisions-only model is offered to the roles that write")
}

func TestProviderCustomHeaders(t *testing.T) {
	p := &Provider{}
	assert.Nil(t, p.CustomHeaders())
	assert.Empty(t, p.HeaderNames())

	require.NoError(t, p.SetCustomHeaders(map[string]string{"X-Two": "2", "X-One": "1"}))
	assert.Equal(t, map[string]string{"X-One": "1", "X-Two": "2"}, p.CustomHeaders())
	assert.Equal(t, []string{"X-One", "X-Two"}, p.HeaderNames())

	// clearing removes the stored JSON
	require.NoError(t, p.SetCustomHeaders(nil))
	assert.Empty(t, p.HeadersJSON)
	assert.Nil(t, p.CustomHeaders())
}

func TestProviderCustomHeadersIgnoresCorruptJSON(t *testing.T) {
	p := &Provider{HeadersJSON: "{not json"}
	assert.Nil(t, p.CustomHeaders())
	assert.Empty(t, p.HeaderNames())
}

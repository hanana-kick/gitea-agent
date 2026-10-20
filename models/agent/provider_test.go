// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

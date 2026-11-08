// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"strings"
	"testing"

	agent_model "gitea.dev/models/agent"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeGroupName(t *testing.T) {
	assert.Equal(t, "Code Review", agent_model.NormalizeGroupName("  Code   Review "))
	assert.Equal(t, "코드 리뷰", agent_model.NormalizeGroupName("코드   리뷰"))
	assert.Empty(t, agent_model.NormalizeGroupName("   "))
}

func TestGroupSlugFromName(t *testing.T) {
	assert.Equal(t, "code-review", agent_model.GroupSlugFromName("Code Review"))
	assert.Equal(t, "code-review-v2", agent_model.GroupSlugFromName("Code Review v2"))
	assert.Equal(t, "a-b-c", agent_model.GroupSlugFromName("  A / B : C  "))
	assert.Equal(t, "gpt-4o-mini", agent_model.GroupSlugFromName("gpt-4o-mini"))

	// names without a readable latin word yield no identifier: the service generates one
	assert.Empty(t, agent_model.GroupSlugFromName("코드 리뷰 (고성능)"))
	assert.Empty(t, agent_model.GroupSlugFromName("무료 모음 2"))
	assert.Empty(t, agent_model.GroupSlugFromName("2024"))
	assert.Empty(t, agent_model.GroupSlugFromName("   "))

	// the identifier stays within the column size
	long := agent_model.GroupSlugFromName(strings.Repeat("word ", 40))
	assert.LessOrEqual(t, len(long), 64)
	assert.False(t, strings.HasPrefix(long, "-"))
	assert.False(t, strings.HasSuffix(long, "-"))
}

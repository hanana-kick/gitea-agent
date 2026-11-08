// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package skills

import (
	"testing"

	agent_model "gitea.dev/models/agent"

	"github.com/stretchr/testify/assert"
)

func TestAllowedForKeepsSkillsInsideTheirReach(t *testing.T) {
	for _, kind := range []string{
		agent_model.ConversationKindResident,
		agent_model.ConversationKindServer,
		agent_model.ConversationKindRepository,
	} {
		assert.True(t, AllowedFor(agent_model.SkillScopeCommon, kind), "a common skill reaches %s", kind)
	}

	// a skill that is about a repository is also for the sessions that look after everything
	assert.True(t, AllowedFor(agent_model.SkillScopeRepository, agent_model.ConversationKindRepository))
	assert.True(t, AllowedFor(agent_model.SkillScopeRepository, agent_model.ConversationKindServer))
	assert.True(t, AllowedFor(agent_model.SkillScopeRepository, agent_model.ConversationKindResident))

	// a skill that is about the whole instance is not for a session that sees one repository
	assert.True(t, AllowedFor(agent_model.SkillScopeServer, agent_model.ConversationKindServer))
	assert.True(t, AllowedFor(agent_model.SkillScopeServer, agent_model.ConversationKindResident))
	assert.False(t, AllowedFor(agent_model.SkillScopeServer, agent_model.ConversationKindRepository))

	// an unknown scope is treated as a common one, so it never widens by accident
	assert.True(t, AllowedFor("nonsense", agent_model.ConversationKindRepository))
	// an unknown session kind reaches no further than a common skill, like it does for tools
	assert.True(t, AllowedFor(agent_model.SkillScopeCommon, "nonsense"))
	assert.False(t, AllowedFor(agent_model.SkillScopeRepository, "nonsense"))
	assert.False(t, AllowedFor(agent_model.SkillScopeServer, "nonsense"))
}

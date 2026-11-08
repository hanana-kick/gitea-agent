// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Without an admin row a tool falls back to what it is for: a read runs on the model's
// decision, a write stays off until an admin switches it on.
func TestPermissionOfAToolWithoutAnAdminRow(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	mode, enabled, err := ModeOf(ctx, readFileToolName)
	require.NoError(t, err)
	assert.Equal(t, PermissionLLM, mode)
	assert.True(t, enabled)

	// a write waits for an admin, so a fresh instance lets no agent change anything
	mode, enabled, err = ModeOf(ctx, "gitea_create_branch")
	require.NoError(t, err)
	assert.Equal(t, PermissionManual, mode)
	assert.False(t, enabled)

	// nothing answers to a name outside the registry, so it reads as the model-decides default
	mode, enabled, err = ModeOf(ctx, "gitea_nonesuch")
	require.NoError(t, err)
	assert.Equal(t, PermissionLLM, mode)
	assert.True(t, enabled)
}

// A mode the admin page no longer knows reads as the model-decides one, keeping the switch
// the admin set.
func TestPermissionOfARowWhoseModeIsNotRecognised(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	require.NoError(t, SetMode(ctx, "gitea_read_file", "unknown-mode", false))

	mode, enabled, err := ModeOf(ctx, "gitea_read_file")
	require.NoError(t, err)
	assert.Equal(t, PermissionLLM, mode)
	assert.False(t, enabled)
}

// Check answers for the session it is given: which tools may run here, which wait for their
// reader, and which are not callable at all.
func TestPermissionCheckDecidesWhetherACallMayProceed(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	repository := Session{Kind: agent_model.ConversationKindRepository, RepoID: 1}

	cases := []struct {
		name    string
		session Session
		tool    string
		stored  *agent_model.ToolPermission
		want    Decision
		wantErr bool
	}{
		{
			name:    "a read tool runs on the model's decision",
			session: repository,
			tool:    readFileToolName,
			want:    DecisionRun,
		},
		{
			name:    "a write tool switched to allow_all runs without asking its reader",
			session: repository,
			tool:    "gitea_create_branch",
			stored:  &agent_model.ToolPermission{Mode: PermissionAllowAll, Enabled: true},
			want:    DecisionRun,
		},
		{
			name:    "a write tool left on manual waits for its reader",
			session: repository,
			tool:    "gitea_create_branch",
			stored:  &agent_model.ToolPermission{Mode: PermissionManual, Enabled: true},
			want:    DecisionAsk,
		},
		{
			name:    "a write tool nobody enabled is denied",
			session: repository,
			tool:    "gitea_create_branch",
			want:    DecisionDenied,
		},
		{
			name:    "a tool too wide for the session's reach is denied without a failure",
			session: Session{Kind: "unknown"},
			tool:    readFileToolName,
			want:    DecisionDenied,
		},
		{
			name:    "a tool with nothing to offer this session is denied",
			session: repository,
			tool:    "gitea_list_repos",
			want:    DecisionDenied,
		},
		{
			name:    "a tool an admin switched off is denied wherever it would be offered",
			session: repository,
			tool:    readFileToolName,
			stored:  &agent_model.ToolPermission{Mode: PermissionLLM, Enabled: false},
			want:    DecisionDenied,
		},
		{
			name:    "a call naming no tool is an error",
			session: repository,
			tool:    "   ",
			want:    DecisionDenied,
			wantErr: true,
		},
		{
			name:    "a call naming a tool nothing answers to is an error",
			session: repository,
			tool:    "gitea_nonesuch",
			want:    DecisionDenied,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, unittest.PrepareTestDatabase())
			if tc.stored != nil {
				require.NoError(t, SetMode(ctx, tc.tool, tc.stored.Mode, tc.stored.Enabled))
			}

			_, decision, err := Check(ctx, tc.session, tc.tool)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, decision)
		})
	}
}

// SetMode writes the row an admin page asks for: it inserts the first time and updates in
// place afterwards, never leaving two rows for one tool.
func TestPermissionSetModeInsertsThenUpdatesTheRow(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	require.NoError(t, SetMode(ctx, "gitea_create_branch", PermissionManual, true))
	mode, enabled, err := ModeOf(ctx, "gitea_create_branch")
	require.NoError(t, err)
	assert.Equal(t, PermissionManual, mode)
	assert.True(t, enabled)

	require.NoError(t, SetMode(ctx, "gitea_create_branch", PermissionAllowAll, false))
	mode, enabled, err = ModeOf(ctx, "gitea_create_branch")
	require.NoError(t, err)
	assert.Equal(t, PermissionAllowAll, mode)
	assert.False(t, enabled)

	count, err := db.GetEngine(ctx).Where("tool = ?", "gitea_create_branch").Count(&agent_model.ToolPermission{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
}

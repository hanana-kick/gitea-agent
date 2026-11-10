// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	agent_tools "gitea.dev/services/agent/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A call is answered once: a second reader — or the same one a second time — is told that the
// decision is already in, and the decision that stands is the first one.
func TestApprovalOfACallAlreadyAnsweredIsRefused(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	reader := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	run := &agent_model.Run{ConversationID: 9001, UserID: reader.ID, Status: agent_model.RunStatusWaiting}
	require.NoError(t, db.Insert(ctx, run))
	answered := &agent_model.ToolApproval{
		ConversationID: 9001, RunID: run.ID, Tool: "gitea_read_file", Arguments: `{"path":"README.md"}`,
		Status: agent_model.ToolApprovalRefused, DecidedUserID: 2,
	}
	require.NoError(t, db.Insert(ctx, answered))

	err := ResolveApproval(ctx, 9001, answered.ID, reader, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrApprovalNotPending{})

	stored := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(answered.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalRefused, stored.Status, "the first answer outlives the late one")
}

// A call the reader cannot even see is not answered either: an approval of another session is
// not reachable through this one.
func TestApprovalOfACallOfAnotherSessionIsRefused(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	reader := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	run := &agent_model.Run{ConversationID: 9002, UserID: reader.ID, Status: agent_model.RunStatusWaiting}
	require.NoError(t, db.Insert(ctx, run))
	approval := &agent_model.ToolApproval{
		ConversationID: 9002, RunID: run.ID, Tool: "gitea_read_file", Arguments: "{}",
		Status: agent_model.ToolApprovalPending,
	}
	require.NoError(t, db.Insert(ctx, approval))

	err := ResolveApproval(ctx, 9003, approval.ID, reader, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrApprovalNotPending{})

	stored := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(approval.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalPending, stored.Status, "a call of another session is left for its own reader")
}

// Only the person whose question started the run may answer for it; anyone else is refused and
// the call keeps waiting for the one who may answer.
func TestApprovalBySomeoneOtherThanTheRunsOwnerIsRefused(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	other := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})

	run := &agent_model.Run{ConversationID: 9004, UserID: owner.ID, Status: agent_model.RunStatusWaiting}
	require.NoError(t, db.Insert(ctx, run))
	approval := &agent_model.ToolApproval{
		ConversationID: 9004, RunID: run.ID, Tool: "gitea_commit_files", Arguments: "{}",
		Status: agent_model.ToolApprovalPending,
	}
	require.NoError(t, db.Insert(ctx, approval))

	err := ResolveApproval(ctx, 9004, approval.ID, other, true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrApprovalNotOwner{})

	stored := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(approval.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalPending, stored.Status, "a refused reader leaves the call unanswered")
	assert.Zero(t, stored.DecidedUserID)
	assert.Zero(t, stored.DecidedUnix)

	pending, err := PendingApprovals(ctx, 9004)
	require.NoError(t, err)
	assert.Len(t, pending, 1, "the call is still there for the user who may answer it")
}

// A run that belongs to nobody — UserID 0 — has no owner to guard it, so any reader may answer
// it. This is the behavior of the `run.UserID != 0 &&` guard, pinned here.
func TestApprovalOfARunWithNoOwnerAcceptsAnyDecider(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	decider := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})

	run := &agent_model.Run{ConversationID: 9005, Status: agent_model.RunStatusWaiting}
	require.NoError(t, db.Insert(ctx, run))
	approval := &agent_model.ToolApproval{
		ConversationID: 9005, RunID: run.ID, Tool: "gitea_read_file", Arguments: "{}",
		Status: agent_model.ToolApprovalPending,
	}
	require.NoError(t, db.Insert(ctx, approval))

	require.NoError(t, ResolveApproval(ctx, 9005, approval.ID, decider, false))

	stored := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(approval.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalRefused, stored.Status)
	assert.Equal(t, decider.ID, stored.DecidedUserID, "a run with no owner accepts whoever comes to answer")
	assert.Contains(t, stored.Result, "did not allow")
}

// The admin can switch a tool off while its call waits for a reader: allowing it then must not
// run it, and the run still ends instead of hanging on a call that can no longer happen.
func TestApprovalOfAToolSwitchedOffAfterTheCallDoesNotRunIt(t *testing.T) {
	ctx := context.Background()
	model := newScriptedModel(t, "gitea_read_file", `{"path":"README.md"}`, "the readme is short")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_read_file", agent_tools.PermissionManual, true))
	t.Cleanup(func() {
		_ = agent_tools.SetMode(context.Background(), "gitea_read_file", agent_tools.PermissionLLM, true)
	})

	_, _, err := SubmitUserMessage(ctx, conversation, user.ID, "read the readme")
	require.NoError(t, err)

	deadline := time.Now().Add(20 * time.Second)
	var approval *agent_model.ToolApproval
	for time.Now().Before(deadline) {
		pending, err := PendingApprovals(ctx, conversation.ID)
		require.NoError(t, err)
		if len(pending) > 0 {
			approval = pending[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, approval, "the call was never put to its reader")

	require.NoError(t, agent_tools.SetMode(ctx, "gitea_read_file", agent_tools.PermissionLLM, false))
	require.NoError(t, ResolveApproval(ctx, conversation.ID, approval.ID, user, true))

	deadline = time.Now().Add(20 * time.Second)
	var latest *agent_model.Run
	for time.Now().Before(deadline) {
		latest, err = LatestRun(ctx, conversation.ID)
		require.NoError(t, err)
		if latest != nil && !agent_model.IsRunActive(latest.Status) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, latest)
	require.Equal(t, agent_model.RunStatusSucceeded, latest.Status, latest.Error)

	assert.Empty(t, toolRecord(t, conversation.ID, "gitea_read_file"), "a tool switched off meanwhile must not run")
	final := messagesOf(t, conversation.ID)
	assert.Equal(t, "the readme is short", final[len(final)-1].Content, "the run ends with an answer all the same")

	requests := model.requestsMade()
	require.NotEmpty(t, requests)
	last := requests[len(requests)-1]
	assert.NotContains(t, toolsOffered(last), "gitea_read_file")
	told := ""
	for _, message := range last.Messages {
		if message.Role == "tool" {
			told = message.Content
		}
	}
	assert.Contains(t, told, "switched off", "the model is told why the call it was allowed did not happen")

	settled := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(approval.ID).Get(settled)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalAllowed, settled.Status)
	assert.Contains(t, settled.Result, "switched off")
}

// The calls a session is waiting on are the pending ones, oldest first, and only its own.
func TestPendingApprovalsListOnlyTheWaitingCallsOfASessionInIdOrder(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	insert := func(conversationID int64, tool, status string) *agent_model.ToolApproval {
		approval := &agent_model.ToolApproval{
			ConversationID: conversationID, RunID: 1, Tool: tool, Arguments: "{}", Status: status,
		}
		require.NoError(t, db.Insert(ctx, approval))
		return approval
	}
	first := insert(9006, "gitea_read_file", agent_model.ToolApprovalPending)
	insert(9006, "gitea_read_issue", agent_model.ToolApprovalAllowed)
	third := insert(9006, "gitea_commit_files", agent_model.ToolApprovalPending)
	insert(9007, "gitea_read_file", agent_model.ToolApprovalPending)

	pending, err := PendingApprovals(ctx, 9006)
	require.NoError(t, err)
	require.Len(t, pending, 2, "an answered call is not waiting, and another session's is not this session's")
	assert.Equal(t, []int64{first.ID, third.ID}, []int64{pending[0].ID, pending[1].ID})
	assert.Equal(t, []string{"gitea_read_file", "gitea_commit_files"}, []string{pending[0].Tool, pending[1].Tool})
}

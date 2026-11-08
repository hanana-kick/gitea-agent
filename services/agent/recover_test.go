// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A run the process was in the middle of when it died ends with that in its record, and the
// session it answered in is nobody's to wait on any more.
func TestRecoverRunsFailsTheRunARestartInterrupted(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	conversation := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, OwnerID: 2, RepoID: 1, Title: "interrupted"}
	require.NoError(t, db.Insert(ctx, conversation))
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: 2, Status: agent_model.RunStatusRunning}
	require.NoError(t, db.Insert(ctx, run))

	busy, err := BusyConversationIDs(ctx)
	require.NoError(t, err)
	require.True(t, busy[conversation.ID], "a running run makes its session busy")

	require.NoError(t, recoverRuns(ctx))

	recovered := &agent_model.Run{}
	has, err := db.GetEngine(ctx).ID(run.ID).Get(recovered)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.RunStatusFailed, recovered.Status)
	assert.Equal(t, "interrupted by a server restart", recovered.Error)
	assert.NotZero(t, recovered.FinishedUnix)

	busy, err = BusyConversationIDs(ctx)
	require.NoError(t, err)
	assert.False(t, busy[conversation.ID], "the session stops answering once its run is failed")
}

// A run that waited on a call when the server restarted cannot be resumed by this process: it
// ends, and the call it waited on is closed instead of left pending for nobody.
func TestRecoverRunsExpiresTheApprovalOfAWaitingRun(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	conversation := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, OwnerID: 2, RepoID: 1, Title: "waiting"}
	require.NoError(t, db.Insert(ctx, conversation))
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: 2, Status: agent_model.RunStatusWaiting}
	require.NoError(t, db.Insert(ctx, run))
	approval := &agent_model.ToolApproval{ConversationID: conversation.ID, RunID: run.ID, Tool: "gitea_commit_files", Arguments: "{}", Status: agent_model.ToolApprovalPending}
	require.NoError(t, db.Insert(ctx, approval))

	require.NoError(t, recoverRuns(ctx))

	recovered := &agent_model.Run{}
	has, err := db.GetEngine(ctx).ID(run.ID).Get(recovered)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.RunStatusFailed, recovered.Status)

	closed := &agent_model.ToolApproval{}
	has, err = db.GetEngine(ctx).ID(approval.ID).Get(closed)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalExpired, closed.Status)
	assert.NotZero(t, closed.DecidedUnix)

	active, err := ActiveRun(ctx, conversation.ID)
	require.NoError(t, err)
	assert.Nil(t, active, "the restarted instance has nothing left to resume")
}

// Runs that never started are handed to the workers again, oldest first, so a restart resumes
// work in the order it was asked for rather than the order the rows happen to sit in.
func TestRecoverRunsRequeuesTheRunsThatNeverStarted(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	conversation := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, OwnerID: 2, RepoID: 1, Title: "queued"}
	require.NoError(t, db.Insert(ctx, conversation))
	// the ids are given rather than assigned, so the fill order can be told apart from the id order
	for _, id := range []int64{30, 10, 20} {
		require.NoError(t, db.Insert(ctx, &agent_model.Run{ID: id, ConversationID: conversation.ID, UserID: 2, Status: agent_model.RunStatusQueued}))
	}

	// the queue is drained into a fresh one, so the order the runs were put in can be read back
	previous := engine.queue
	engine.queue = newRunQueue()
	defer func() { engine.queue = previous }()

	require.NoError(t, recoverRuns(ctx))

	drained := make([]int64, 0, 3)
	for _, id := range []int64{10, 20, 30} {
		position, waiting := engine.queue.position(id)
		if waiting {
			assert.Equal(t, len(drained)+1, position, "run %d did not keep its place", id)
			drained = append(drained, id)
		}
	}
	assert.NotEmpty(t, drained, "nothing was re-queued")
	assert.LessOrEqual(t, len(drained), 3)

	for _, id := range []int64{10, 20, 30} {
		run := &agent_model.Run{}
		has, err := db.GetEngine(ctx).ID(id).Get(run)
		require.NoError(t, err)
		require.True(t, has)
		assert.Equal(t, agent_model.RunStatusQueued, run.Status)
	}
}

// A run that is going is stopped through the context its worker gave it: the worker settles the
// run itself, so cancelling only has to ask.
func TestCancelRunStopsAnActiveRunThroughItsContext(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	conversation := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, OwnerID: 2, RepoID: 1, Title: "active"}
	require.NoError(t, db.Insert(ctx, conversation))
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: 2, Status: agent_model.RunStatusRunning}
	require.NoError(t, db.Insert(ctx, run))

	runCtx, cancel := context.WithCancel(context.Background())
	engine.setCancel(conversation.ID, cancel)
	defer engine.clearCancel(conversation.ID)

	require.NoError(t, CancelRun(conversation.ID))
	require.ErrorIs(t, runCtx.Err(), context.Canceled)

	recovered := &agent_model.Run{}
	has, err := db.GetEngine(ctx).ID(run.ID).Get(recovered)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.RunStatusRunning, recovered.Status, "the worker the context stops settles the run")
}

// A run still in the queue is not held by a worker: cancelling ends it here, and the worker that
// later finds it has nothing left to do.
func TestCancelRunFinishesAQueuedRunNoWorkerHolds(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	conversation := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, OwnerID: 2, RepoID: 1, Title: "queued"}
	require.NoError(t, db.Insert(ctx, conversation))
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: 2, Status: agent_model.RunStatusQueued}
	require.NoError(t, db.Insert(ctx, run))

	require.NoError(t, CancelRun(conversation.ID))

	recovered := &agent_model.Run{}
	has, err := db.GetEngine(ctx).ID(run.ID).Get(recovered)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.RunStatusCanceled, recovered.Status)
	assert.NotZero(t, recovered.FinishedUnix)
}

// Cancelling a run that waits for its reader also answers the question it asked: the run is
// canceled and its call is closed, so no reader is left deciding a call nobody waits for.
func TestCancelRunFinishesAWaitingRunAndExpiresItsApproval(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	conversation := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, OwnerID: 2, RepoID: 1, Title: "waiting"}
	require.NoError(t, db.Insert(ctx, conversation))
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: 2, Status: agent_model.RunStatusWaiting}
	require.NoError(t, db.Insert(ctx, run))
	approval := &agent_model.ToolApproval{ConversationID: conversation.ID, RunID: run.ID, Tool: "gitea_commit_files", Arguments: "{}", Status: agent_model.ToolApprovalPending}
	require.NoError(t, db.Insert(ctx, approval))

	require.NoError(t, CancelRun(conversation.ID))

	recovered := &agent_model.Run{}
	has, err := db.GetEngine(ctx).ID(run.ID).Get(recovered)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.RunStatusCanceled, recovered.Status)
	assert.NotZero(t, recovered.FinishedUnix)

	closed := &agent_model.ToolApproval{}
	has, err = db.GetEngine(ctx).ID(approval.ID).Get(closed)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalExpired, closed.Status)
	assert.NotZero(t, closed.DecidedUnix)
}

// A session with nothing going on has nothing to cancel, and says so.
func TestCancelRunWithNothingActiveReportsIt(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		status string
	}{
		{name: "a session that never ran anything"},
		{name: "a session whose only run already ended", status: agent_model.RunStatusSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conversation := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, OwnerID: 2, RepoID: 1, Title: "idle"}
			require.NoError(t, db.Insert(ctx, conversation))
			if tc.status != "" {
				require.NoError(t, db.Insert(ctx, &agent_model.Run{ConversationID: conversation.ID, UserID: 2, Status: tc.status}))
			}
			assert.ErrorIs(t, CancelRun(conversation.ID), ErrNoActiveRun{})
		})
	}
}

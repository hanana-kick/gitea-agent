// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strandedRun stores a run in one of the states nothing works on any more. What each state
// means for a reader is the point: a run left here is a request that never ends.
func strandedRun(t *testing.T, status string, ago time.Duration, started bool) *agent_model.Run {
	t.Helper()
	ctx := context.Background()
	conversation, err := CreateConversation(ctx, agent_model.ConversationKindRepository, 2, 1, "stranded")
	require.NoError(t, err)
	at := timeutil.TimeStampNow() - timeutil.TimeStamp(int64(ago.Seconds()))
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: 2, Status: status}
	if started {
		run.StartedUnix = at
	}
	require.NoError(t, db.Insert(ctx, run))
	// the created stamp is the database's on insert, so a run that lay there for a while has to
	// be aged after it is stored
	_, err = db.GetEngine(ctx).ID(run.ID).Cols("created_unix", "started_unix").
		Update(&agent_model.Run{CreatedUnix: at, StartedUnix: run.StartedUnix})
	require.NoError(t, err)
	run.CreatedUnix = at
	return run
}

func runStatus(t *testing.T, runID int64) (*agent_model.Run, bool) {
	t.Helper()
	found := &agent_model.Run{}
	has, err := db.GetEngine(context.Background()).ID(runID).Get(found)
	require.NoError(t, err)
	return found, has
}

// A run nothing is working on is finished by the sweep: a reader sees progress, a failure or
// an answer, never a request that rests where nothing will move it.
func TestSweepFinishesRunsNothingIsWorkingOn(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	stale := engineRunTimeout + sweepGrace + time.Minute

	cases := []struct {
		name   string
		status string
		reason string
	}{
		{"running", agent_model.RunStatusRunning, "did not answer in time"},
		{"queued", agent_model.RunStatusQueued, "never started"},
		{"waiting", agent_model.RunStatusWaiting, "call it waited for is gone"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			run := strandedRun(t, testCase.status, stale, testCase.status != agent_model.RunStatusQueued)
			require.NoError(t, sweepRuns(context.Background()))

			found, has := runStatus(t, run.ID)
			require.True(t, has)
			assert.Equal(t, agent_model.RunStatusFailed, found.Status)
			assert.Contains(t, found.Error, testCase.reason)
			assert.NotZero(t, found.FinishedUnix)
		})
	}
}

// A run that is going, or waits on a call that is really there, is not touched: the sweep
// only ends what nothing can end.
func TestSweepLeavesRunsThatAreGoingAlone(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	stale := engineRunTimeout + sweepGrace + time.Minute

	freshRunning := strandedRun(t, agent_model.RunStatusRunning, time.Minute, true)
	freshQueued := strandedRun(t, agent_model.RunStatusQueued, time.Minute, false)
	oldRunning := strandedRun(t, agent_model.RunStatusRunning, stale, true)
	waiting := strandedRun(t, agent_model.RunStatusWaiting, stale, true)
	require.NoError(t, db.Insert(context.Background(), &agent_model.ToolApproval{
		ConversationID: waiting.ConversationID, RunID: waiting.ID, Tool: "gitea_commit_files",
		Arguments: "{}", Status: agent_model.ToolApprovalPending,
	}))

	require.NoError(t, sweepRuns(context.Background()))

	for _, run := range []*agent_model.Run{freshRunning, freshQueued, waiting} {
		found, has := runStatus(t, run.ID)
		require.True(t, has)
		assert.Equal(t, run.Status, found.Status, "run %d was finished by the sweep", run.ID)
	}
	// the one the sweep was there for is still finished
	finished, _ := runStatus(t, oldRunning.ID)
	assert.Equal(t, agent_model.RunStatusFailed, finished.Status)
}

// A run a worker is still holding is left alone, however long it takes: the sweep ends lost
// work, and a slow answer is not lost work.
func TestSweepLeavesARunAWorkerIsHolding(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	run := strandedRun(t, agent_model.RunStatusRunning, engineRunTimeout+sweepGrace+time.Minute, true)

	lock := engine.conversationLock(run.ConversationID)
	lock.Lock()
	require.NoError(t, sweepRuns(ctx))
	held, _ := runStatus(t, run.ID)
	assert.Equal(t, agent_model.RunStatusRunning, held.Status, "a run a worker holds was finished by the sweep")
	lock.Unlock()

	require.NoError(t, sweepRuns(ctx))
	freed, _ := runStatus(t, run.ID)
	assert.Equal(t, agent_model.RunStatusFailed, freed.Status, "the sweep did not finish the run once it was free")
}

// A run whose reader no longer exists cannot be answered by anyone else, so the sweep ends it
// rather than leaving a request that waits for a reader who will not come back.
func TestSweepFinishesARunWhoseReaderIsGone(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	run := strandedRun(t, agent_model.RunStatusWaiting, engineRunTimeout+sweepGrace+time.Minute, true)
	run.UserID = 4242 // no such account
	_, err := db.GetEngine(ctx).ID(run.ID).Cols("user_id").Update(run)
	require.NoError(t, err)
	require.NoError(t, db.Insert(ctx, &agent_model.ToolApproval{
		ConversationID: run.ConversationID, RunID: run.ID, Tool: "gitea_commit_files",
		Arguments: "{}", Status: agent_model.ToolApprovalPending,
	}))

	require.NoError(t, sweepRuns(ctx))

	found, has := runStatus(t, run.ID)
	require.True(t, has)
	assert.Equal(t, agent_model.RunStatusFailed, found.Status)
	assert.Contains(t, found.Error, "reader who asked is gone")
}

// A run that ends has no call left behind: a pending call would let an answer run a write for
// a request that is over.
func TestAFinishedRunLeavesNoCallBehind(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	run := strandedRun(t, agent_model.RunStatusWaiting, time.Minute, true)
	approval := &agent_model.ToolApproval{
		ConversationID: run.ConversationID, RunID: run.ID, Tool: "gitea_commit_files",
		Arguments: `{"branch":"liveness","message":"x","files":[{"path":"docs/x.md","content":"x"}]}`,
		Status:    agent_model.ToolApprovalPending,
	}
	require.NoError(t, db.Insert(ctx, approval))

	engine.finishRun(ctx, run, agent_model.RunStatusFailed, "the run was stopped")

	found := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(approval.ID).Get(found)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalExpired, found.Status)
	assert.NotZero(t, found.DecidedUnix)

	// the reader can no longer answer it, and nothing runs for it
	require.ErrorIs(t, ResolveApproval(ctx, run.ConversationID, approval.ID, user.ID, true), ErrApprovalNotPending{})
	assert.Empty(t, toolRecord(t, run.ConversationID, "gitea_commit_files"))
	assert.Empty(t, messagesOf(t, run.ConversationID))
}

// A submit that cannot be stored leaves neither the message nor the run: a message with no
// run is a request nothing answers.
func TestASubmitThatFailsStoresNothing(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	conversation, err := CreateConversation(ctx, agent_model.ConversationKindRepository, user.ID, 1, "submit")
	require.NoError(t, err)

	// the message is what a run answers, so a message that cannot be stored stores no run
	_, _, err = SubmitUserMessage(ctx, conversation, user.ID, "   ")
	require.Error(t, err)
	assert.Empty(t, messagesOf(t, conversation.ID), "a refused message was stored")
	active, err := ActiveRun(ctx, conversation.ID)
	require.NoError(t, err)
	assert.Nil(t, active, "a run was left with no message to answer")

	// what a submit stores is stored in one transaction, so a failure after the message is
	// written takes the message with it
	err = db.WithTx(ctx, func(ctx context.Context) error {
		if _, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleUser, "halfway", ""); err != nil {
			return err
		}
		return errors.New("the run could not be started")
	})
	require.Error(t, err)
	assert.Empty(t, messagesOf(t, conversation.ID), "a message was left behind by a failed submit")

	// and the pair that is stored comes together
	message, run, err := SubmitUserMessage(ctx, conversation, user.ID, "hello there")
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.NotZero(t, run.ID)
	assert.Equal(t, conversation.ID, run.ConversationID)
	assert.Equal(t, agent_model.RunStatusQueued, run.Status)
	assert.Contains(t, idsOf(messagesOf(t, conversation.ID)), message.ID)
	// the run answers that message: a message always has the run it asked for
	stored := &agent_model.Message{}
	has, err := db.GetEngine(ctx).Where("conversation_id = ?", conversation.ID).
		And("role = ?", agent_model.MessageRoleUser).OrderBy("id DESC").Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, message.ID, stored.ID)
}

func idsOf(messages []*agent_model.Message) []int64 {
	ids := make([]int64, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}

// How many sessions are answered at once follows the instance's setting, and never leaves the
// agent with no worker or with more than the cap.
func TestTheNumberOfConcurrentSessionsFollowsTheSetting(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer func(previous int) { setting.Agent.MaxConcurrentSessions = previous }(setting.Agent.MaxConcurrentSessions)

	cases := []struct {
		setting int
		workers int
	}{
		{0, engineWorkers},
		{-3, engineWorkers},
		{1, 1},
		{7, 7},
		{9999, engineMaxWorkers},
	}
	for _, testCase := range cases {
		setting.Agent.MaxConcurrentSessions = testCase.setting
		assert.Equal(t, testCase.workers, engineWorkerCount(), "setting %d", testCase.setting)
	}
}

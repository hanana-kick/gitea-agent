// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"sync"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The queue hands runs out in the order they arrived, and says where each one stands: a reader
// of a waiting run sees its place, not only that it waits.
func TestTheQueueKeepsTheOrderAndThePlaceOfEveryRun(t *testing.T) {
	queue := newRunQueue()
	for _, id := range []int64{7, 8, 9} {
		queue.push(id, false)
	}

	assert.Equal(t, 3, queue.waiting())
	for index, id := range []int64{7, 8, 9} {
		position, waiting := queue.position(id)
		assert.True(t, waiting, "run %d is not in the queue", id)
		assert.Equal(t, index+1, position)
	}
	_, waiting := queue.position(99)
	assert.False(t, waiting, "a run that was never queued has a place")

	assert.Equal(t, int64(7), queue.take())
	assert.Equal(t, 2, queue.waiting())
	position, waiting := queue.position(8)
	assert.True(t, waiting)
	assert.Equal(t, 1, position, "the run behind did not move up")
	assert.Equal(t, int64(8), queue.take())
	assert.Equal(t, int64(9), queue.take())
	assert.Equal(t, 0, queue.waiting())
}

// The queue holds as many runs as arrive: an instance under load makes them wait, it does not
// drop the ones at the end.
func TestTheQueueHoldsEveryRunItIsGiven(t *testing.T) {
	queue := newRunQueue()
	ids := make([]int64, 0, 1000)
	for id := int64(1); id <= 1000; id++ {
		queue.push(id, false)
		ids = append(ids, id)
	}

	assert.Equal(t, len(ids), queue.waiting())
	last := int64(1000)
	position, waiting := queue.position(last)
	assert.True(t, waiting)
	assert.Equal(t, len(ids), position, "the last run does not stand at the end")
	for _, id := range ids {
		assert.Equal(t, id, queue.take(), "run %d did not come out in its turn", id)
	}
	assert.Equal(t, 0, queue.waiting())
}

// A worker waits for work rather than spinning, and every run pushed to an idle queue is taken.
func TestAWorkerTakesWhatIsPushedWhileItWaits(t *testing.T) {
	queue := newRunQueue()
	taken := make(chan int64, 3)
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			taken <- queue.take()
		})
	}

	for _, id := range []int64{1, 2, 3} {
		queue.push(id, false)
	}
	workers.Wait()
	close(taken)

	got := make([]int64, 0, 3)
	for id := range taken {
		got = append(got, id)
	}
	assert.ElementsMatch(t, []int64{1, 2, 3}, got)
}

// The state a client is handed carries the queue situation of a waiting run, and nothing else
// claims one.
func TestTheRunStateCarriesThePlaceOfAWaitingRun(t *testing.T) {
	previous := engine.queue
	engine.queue = newRunQueue()
	defer func() { engine.queue = previous }()

	queued := &agent_model.Run{ID: 42, ConversationID: 1, Status: agent_model.RunStatusQueued}
	engine.queue.push(42, false)
	engine.queue.push(43, false)

	state := RunStateOf(queued)
	assert.Equal(t, 1, state.QueuePosition)
	assert.Equal(t, 2, state.QueueWaiting)
	assert.True(t, state.Cancelable)

	// a run that is not waiting has no place, whatever the queue holds
	running := &agent_model.Run{ID: 44, ConversationID: 1, Status: agent_model.RunStatusRunning}
	assert.Zero(t, RunStateOf(running).QueuePosition)
	assert.Zero(t, RunStateOf(running).QueueWaiting)
	// and neither has a waiting run the queue does not know
	assert.Zero(t, RunStateOf(&agent_model.Run{ID: 99, Status: agent_model.RunStatusQueued}).QueuePosition)
}

// The always-on session goes ahead of the queue: an instance under load keeps looking after
// itself while readers wait, and everyone else still waits in the order they arrived.
func TestTheAlwaysOnSessionGoesAheadOfTheQueue(t *testing.T) {
	queue := newRunQueue()
	queue.push(1, false)
	queue.push(2, false)
	queue.push(3, false)
	queue.push(10, true)

	assert.Equal(t, 4, queue.waiting())
	assert.Equal(t, 1, queue.priorityWaiting())

	// the run of the always-on session is first, and the others count it as waiting ahead
	position, waiting := queue.position(10)
	assert.True(t, waiting)
	assert.Equal(t, 1, position)
	position, _ = queue.position(1)
	assert.Equal(t, 2, position, "a reader's run does not count the priority lane")
	position, _ = queue.position(3)
	assert.Equal(t, 4, position)

	assert.Equal(t, int64(10), queue.take())
	assert.Equal(t, int64(1), queue.take())
	position, _ = queue.position(2)
	assert.Equal(t, 1, position, "the queue did not close up after the priority run")
}

// A resident session's run is queued ahead of a reader's, whatever kind of session asks.
func TestAResidentSessionRunIsQueuedAhead(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	previous := engine.queue
	engine.queue = newRunQueue()
	defer func() { engine.queue = previous }()

	resident := &agent_model.Conversation{Kind: agent_model.ConversationKindResident, Title: "resident"}
	repository := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, RepoID: 1, Title: "repository"}
	require.NoError(t, db.Insert(ctx, resident))
	require.NoError(t, db.Insert(ctx, repository))

	engine.startQueuedRun(repository, &agent_model.Run{ID: 501, ConversationID: repository.ID, Status: agent_model.RunStatusQueued})
	engine.startQueuedRun(resident, &agent_model.Run{ID: 502, ConversationID: resident.ID, Status: agent_model.RunStatusQueued})

	position, waiting := engine.queue.position(502)
	assert.True(t, waiting)
	assert.Equal(t, 1, position, "the resident session's run is not first")
	position, _ = engine.queue.position(501)
	assert.Equal(t, 2, position)
}

// A waiting run says how long it has already waited: what a run costs cannot be known, but the
// wait itself is a fact, and a run that is not waiting says nothing about one.
func TestAWaitingRunSaysHowLongItHasWaited(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	previous := engine.queue
	engine.queue = newRunQueue()
	defer func() { engine.queue = previous }()

	conversation := &agent_model.Conversation{Kind: agent_model.ConversationKindRepository, RepoID: 1, Title: "queue"}
	require.NoError(t, db.Insert(ctx, conversation))
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: 2, Status: agent_model.RunStatusQueued}
	require.NoError(t, db.Insert(ctx, run))
	// the run has been waiting since it was stored, so age it the way a long wait would
	created := timeutil.TimeStampNow() - 120
	_, err := db.GetEngine(ctx).ID(run.ID).Cols("created_unix").Update(&agent_model.Run{CreatedUnix: created})
	require.NoError(t, err)
	run.CreatedUnix = created

	engine.queue.push(run.ID, false)
	state := RunStateOf(run)
	assert.Equal(t, 1, state.QueuePosition)
	assert.Equal(t, 1, state.QueueWaiting)
	assert.Equal(t, created.AsTime().Unix(), state.QueueSince)
	assert.InDelta(t, 120, state.QueueWaitedSeconds, 2)

	running := &agent_model.Run{ID: 99, ConversationID: conversation.ID, Status: agent_model.RunStatusRunning}
	assert.Zero(t, RunStateOf(running).QueueWaitedSeconds, "a run that is not waiting reports a wait")
	assert.Zero(t, RunStateOf(running).QueueSince)
}

// Which sessions go ahead of the queue is the instance's choice, and the default is the
// always-on one.
func TestThePriorityKindsFollowTheInstanceSetting(t *testing.T) {
	previous := setting.Agent.PriorityKinds
	defer func() { setting.Agent.PriorityKinds = previous }()

	setting.Agent.PriorityKinds = nil
	assert.True(t, runIsPriority(agent_model.ConversationKindResident), "the default is not the always-on session")
	assert.False(t, runIsPriority(agent_model.ConversationKindServer))

	setting.Agent.PriorityKinds = []string{agent_model.ConversationKindServer}
	assert.True(t, runIsPriority(agent_model.ConversationKindServer))
	assert.False(t, runIsPriority(agent_model.ConversationKindResident))

	setting.Agent.PriorityKinds = []string{"nonsense"}
	assert.False(t, runIsPriority(agent_model.ConversationKindRepository))
}

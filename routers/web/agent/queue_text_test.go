// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	agent_model "gitea.dev/models/agent"
	agent_service "gitea.dev/services/agent"

	"github.com/stretchr/testify/assert"
)

// A run waiting for a worker says where it stands, so a reader can tell a queue from a stall; a
// run whose place the queue does not know yet only says that it waits.
func TestAWaitingRunSaysWhereItStands(t *testing.T) {
	labels := map[string]string{
		"queued":              "Waiting for its turn\u2026",
		"queuedAt":            "%d of %d in the queue",
		"queuedWaitedSeconds": "waiting %d s",
		"queuedWaitedMinutes": "waiting %d min",
	}
	assert.Equal(t, "Waiting for its turn\u2026", queuedText(labels, agent_service.RunState{}))
	assert.Equal(t,
		"Waiting for its turn\u2026 (3 of 7 in the queue)",
		queuedText(labels, agent_service.RunState{
			Status: agent_model.RunStatusQueued, QueuePosition: 3, QueueWaiting: 7,
		}))
	// the wait is a fact: seconds while that reads better, minutes past
	assert.Equal(t,
		"Waiting for its turn\u2026 (3 of 7 in the queue, waiting 40 s)",
		queuedText(labels, agent_service.RunState{QueuePosition: 3, QueueWaiting: 7, QueueWaitedSeconds: 40}))
	assert.Equal(t,
		"Waiting for its turn\u2026 (1 of 2 in the queue, waiting 3 min)",
		queuedText(labels, agent_service.RunState{QueuePosition: 1, QueueWaiting: 2, QueueWaitedSeconds: 180}))

	state, text := pendingState(&agent_model.Run{ID: 1, Status: agent_model.RunStatusQueued}, labels)
	assert.Equal(t, "queued", state)
	assert.Equal(t, "Waiting for its turn\u2026", text)
}

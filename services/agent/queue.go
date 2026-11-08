// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import "sync"

// runQueue is the work waiting for a worker: every run keeps its place in the order it arrived,
// and a run that has to wait says where it is. The queue has no capacity on purpose — an
// instance under load waits, it does not drop or fail the requests behind it.
type runQueue struct {
	mu   sync.Mutex
	cond *sync.Cond
	// first is the lane of the always-on session, which an instance under load must not make
	// wait behind a reader's chat; pending is everyone else, in the order they arrived.
	first   []int64
	pending []int64
}

func newRunQueue() *runQueue {
	queue := &runQueue{}
	queue.cond = sync.NewCond(&queue.mu)
	return queue
}

// push puts a run at the end of its lane: priority runs go ahead of the others.
func (q *runQueue) push(runID int64, priority bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if priority {
		q.first = append(q.first, runID)
	} else {
		q.pending = append(q.pending, runID)
	}
	q.cond.Signal()
}

// take waits for a run to arrive and returns the one that has waited longest, from the priority
// lane first.
func (q *runQueue) take() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.first) == 0 && len(q.pending) == 0 {
		q.cond.Wait()
	}
	if len(q.first) > 0 {
		runID := q.first[0]
		q.first = q.first[1:]
		if len(q.first) == 0 {
			q.first = nil // the emptied slice would otherwise hold on to its whole array
		}
		return runID
	}
	runID := q.pending[0]
	q.pending = q.pending[1:]
	if len(q.pending) == 0 {
		q.pending = nil
	}
	return runID
}

// position is where a run stands among all the ones waiting, counting from one, and whether it
// is waiting at all. A run behind the priority lane waits for it too.
func (q *runQueue) position(runID int64) (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for index, queued := range q.first {
		if queued == runID {
			return index + 1, true
		}
	}
	for index, queued := range q.pending {
		if queued == runID {
			return len(q.first) + index + 1, true
		}
	}
	return 0, false
}

// waiting is how many runs wait for a worker.
func (q *runQueue) waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.first) + len(q.pending)
}

// priorityWaiting is how many runs of the always-on session wait, which is what a run of its
// own kind is put ahead of.
func (q *runQueue) priorityWaiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.first)
}

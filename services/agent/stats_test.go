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
	"gitea.dev/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func modelStat(stats *RunStats, model string) *RunStatModel {
	for i := range stats.Models {
		if stats.Models[i].Model == model {
			return &stats.Models[i]
		}
	}
	return nil
}

// The summary counts what the rows say: how the runs ended, what answered them, how long the work
// and the wait took, and what came of what a reader was asked. A run still going is counted as
// going, not as one that answered.
func TestRunStatsAddUpTheRuns(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	// an hour ago, so "everything" and "today" read differently
	base := timeutil.TimeStamp(time.Now().Add(-time.Hour).Unix())
	runs := []*agent_model.Run{
		{ConversationID: 1, UserID: 2, Status: "succeeded", Model: "stub-a", CreatedUnix: base, StartedUnix: base + 5, FinishedUnix: base + 65},
		{ConversationID: 1, UserID: 2, Status: "succeeded", Model: "stub-a", CreatedUnix: base, StartedUnix: base + 5, FinishedUnix: base + 25},
		{ConversationID: 1, UserID: 2, Status: "failed", Model: "stub-b", CreatedUnix: base, StartedUnix: base + 5, FinishedUnix: base + 15},
		{ConversationID: 1, UserID: 2, Status: "canceled", Model: "stub-b", CreatedUnix: base, StartedUnix: base + 1, FinishedUnix: base + 2},
		{ConversationID: 1, UserID: 2, Status: "running", Model: "stub-b", CreatedUnix: base, StartedUnix: base + 5},
	}
	for _, run := range runs {
		require.NoError(t, db.Insert(ctx, run))
		// the column stamps itself when it is written, so the moment the run was asked for is set
		// afterwards to be the one this spec adds up
		_, err := db.GetEngine(ctx).Exec("UPDATE agent_run SET created_unix = ? WHERE id = ?", base, run.ID)
		require.NoError(t, err)
	}
	approvals := []*agent_model.ToolApproval{
		{ConversationID: 1, RunID: runs[0].ID, Tool: "gitea_commit_files", Status: "allowed", Kind: "approval"},
		{ConversationID: 1, RunID: runs[0].ID, Tool: "gitea_commit_files", Status: "refused", Kind: "approval"},
		{ConversationID: 1, RunID: runs[1].ID, Tool: "gitea_ask_user", Status: "allowed", Kind: "question"},
	}
	for _, approval := range approvals {
		require.NoError(t, db.Insert(ctx, approval))
		_, err := db.GetEngine(ctx).Exec("UPDATE agent_tool_approval SET created_unix = ? WHERE id = ?", base, approval.ID)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		for _, run := range runs {
			_, _ = db.DeleteByID[agent_model.Run](context.Background(), run.ID)
		}
		for _, approval := range approvals {
			_, _ = db.DeleteByID[agent_model.ToolApproval](context.Background(), approval.ID)
		}
	})

	stats, err := RunStatsSince(ctx, time.Unix(int64(base)-60, 0))
	require.NoError(t, err)

	assert.EqualValues(t, 5, stats.Total)
	assert.EqualValues(t, 2, stats.Count("succeeded"))
	assert.EqualValues(t, 1, stats.Count("failed"))
	assert.EqualValues(t, 1, stats.Count("canceled"))
	assert.EqualValues(t, 1, stats.Count("running"))
	assert.EqualValues(t, 0, stats.Count("waiting"))
	assert.EqualValues(t, 4, stats.Finished, "a run still going was counted as one that finished")
	assert.EqualValues(t, 50, stats.SuccessRate(), "two of the four runs that finished answered")

	// 60 + 20 + 10 + 1 seconds of work, over four runs, and 60 as the longest
	assert.Equal(t, 22*time.Second, stats.AverageTime)
	assert.Equal(t, 60*time.Second, stats.LongestTime)
	assert.Equal(t, 4*time.Second, stats.AverageWait)

	if model := modelStat(stats, "stub-a"); assert.NotNil(t, model) {
		assert.EqualValues(t, 2, model.Runs)
		assert.EqualValues(t, 2, model.Succeeded)
		assert.EqualValues(t, 0, model.Failed)
	}
	if model := modelStat(stats, "stub-b"); assert.NotNil(t, model) {
		assert.EqualValues(t, 3, model.Runs)
		assert.EqualValues(t, 0, model.Succeeded)
		assert.EqualValues(t, 1, model.Failed)
	}

	assert.EqualValues(t, 2, stats.ApprovalCount("allowed"))
	assert.EqualValues(t, 1, stats.ApprovalCount("refused"))
	require.NotEmpty(t, stats.Tools)
	assert.Equal(t, "gitea_commit_files", stats.Tools[0].Tool, "the tool asked for most is not first")
	assert.EqualValues(t, 2, stats.Tools[0].Total)
	assert.EqualValues(t, 1, stats.Tools[0].Allowed)
	assert.EqualValues(t, 1, stats.Tools[0].Refused)

	// a window that starts after the runs holds none of them
	older, err := RunStatsSince(ctx, time.Unix(int64(base)+3600, 0))
	require.NoError(t, err)
	assert.EqualValues(t, 0, older.Total)
	assert.EqualValues(t, 0, older.SuccessRate())
}

// The window a page asks for is a number of days, or everything when it names none.
func TestRunStatsWindow(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), RunStatsWindow(7, now))
	assert.Equal(t, time.Unix(0, 0), RunStatsWindow(0, now))
}

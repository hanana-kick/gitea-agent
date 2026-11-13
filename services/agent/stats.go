// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"time"

	"gitea.dev/models/db"
)

// RunStats is what a window of runs adds up to: how they ended, what answered them, how long the
// work took, and what a reader was asked along the way. It is read from the rows themselves, so an
// instance keeps no counters of its own that could drift from what happened.
type RunStats struct {
	Since   time.Time
	Total   int64
	ByState []RunStatCount // one per state a run can rest in, in the order of the states
	// Finished is how many runs came to an end, and Succeeded how many of those answered: a run
	// still going, or waiting for a reader, is neither.
	Finished  int64
	Succeeded int64
	// AverageTime and LongestTime are the work itself, from a run taking its worker to it finishing.
	AverageTime time.Duration
	LongestTime time.Duration
	// AverageWait is how long a run waited for a worker before it started.
	AverageWait time.Duration
	Models      []RunStatModel
	Tools       []RunStatTool
	Approvals   []RunStatCount // what came of what a reader was asked
}

// RunStatCount is one name and how many of it were seen.
type RunStatCount struct {
	Name  string
	Count int64
}

// RunStatModel is what one model did in the window.
type RunStatModel struct {
	Model     string
	Runs      int64
	Succeeded int64
	Failed    int64
}

// RunStatTool is what one tool was asked for in the window, and what came of it.
type RunStatTool struct {
	Tool    string
	Total   int64
	Allowed int64
	Refused int64
}

// runStates is every state a run can rest in, in the order the page lists them.
var runStates = []string{"queued", "running", "waiting", "succeeded", "failed", "canceled"}

// RunStatsSince adds up the runs created since a moment, and what went with them.
func RunStatsSince(ctx context.Context, since time.Time) (*RunStats, error) {
	engine := db.GetEngine(ctx)
	stats := &RunStats{Since: since}

	rows := make([]struct {
		Status string `xorm:"status"`
		Count  int64  `xorm:"count"`
	}, 0, len(runStates))
	if err := engine.Select("status, COUNT(*) AS count").Table("agent_run").
		Where("created_unix >= ?", since.Unix()).GroupBy("status").Find(&rows); err != nil {
		return nil, fmt.Errorf("count runs by state: %w", err)
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		counts[row.Status] = row.Count
		stats.Total += row.Count
	}
	for _, state := range runStates {
		stats.ByState = append(stats.ByState, RunStatCount{Name: state, Count: counts[state]})
	}
	stats.Succeeded = counts["succeeded"]
	stats.Finished = counts["succeeded"] + counts["failed"] + counts["canceled"]

	// a run that finished, and a run that started: what is left of either is not added up
	durations := make([]struct {
		Total   int64 `xorm:"total"`
		Count   int64 `xorm:"count"`
		Longest int64 `xorm:"longest"`
	}, 0, 1)
	if err := engine.Select("SUM(finished_unix - started_unix) AS total, COUNT(*) AS count, MAX(finished_unix - started_unix) AS longest").
		Table("agent_run").Where("created_unix >= ?", since.Unix()).
		Where("finished_unix > 0 AND started_unix > 0").Find(&durations); err != nil {
		return nil, fmt.Errorf("add up run durations: %w", err)
	}
	if len(durations) > 0 && durations[0].Count > 0 {
		stats.AverageTime = time.Duration(durations[0].Total/durations[0].Count) * time.Second
		stats.LongestTime = time.Duration(durations[0].Longest) * time.Second
	}
	waits := make([]struct {
		Total int64 `xorm:"total"`
		Count int64 `xorm:"count"`
	}, 0, 1)
	if err := engine.Select("SUM(started_unix - created_unix) AS total, COUNT(*) AS count").
		Table("agent_run").Where("created_unix >= ?", since.Unix()).
		Where("started_unix > 0").Find(&waits); err != nil {
		return nil, fmt.Errorf("add up run waits: %w", err)
	}
	if len(waits) > 0 && waits[0].Count > 0 {
		stats.AverageWait = time.Duration(waits[0].Total/waits[0].Count) * time.Second
	}

	models := make([]struct {
		Model     string `xorm:"model"`
		Runs      int64  `xorm:"runs"`
		Succeeded int64  `xorm:"succeeded"`
		Failed    int64  `xorm:"failed"`
	}, 0, runStatsLimit)
	if err := engine.Select("model, COUNT(*) AS runs, "+
		"SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END) AS succeeded, "+
		"SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) AS failed").
		Table("agent_run").Where("created_unix >= ?", since.Unix()).
		GroupBy("model").OrderBy("runs DESC").Limit(runStatsLimit).Find(&models); err != nil {
		return nil, fmt.Errorf("add up the models that answered: %w", err)
	}
	for _, row := range models {
		name := row.Model
		if name == "" {
			name = "—"
		}
		stats.Models = append(stats.Models, RunStatModel{Model: name, Runs: row.Runs, Succeeded: row.Succeeded, Failed: row.Failed})
	}

	approvals := make([]struct {
		Status string `xorm:"status"`
		Count  int64  `xorm:"count"`
	}, 0, 4)
	if err := engine.Select("status, COUNT(*) AS count").Table("agent_tool_approval").
		Where("created_unix >= ?", since.Unix()).GroupBy("status").Find(&approvals); err != nil {
		return nil, fmt.Errorf("count approvals by outcome: %w", err)
	}
	for _, row := range approvals {
		stats.Approvals = append(stats.Approvals, RunStatCount{Name: row.Status, Count: row.Count})
	}

	tools := make([]struct {
		Tool    string `xorm:"tool"`
		Total   int64  `xorm:"total"`
		Allowed int64  `xorm:"allowed"`
		Refused int64  `xorm:"refused"`
	}, 0, runStatsLimit)
	if err := engine.Select("tool, COUNT(*) AS total, "+
		"SUM(CASE WHEN status = 'allowed' THEN 1 ELSE 0 END) AS allowed, "+
		"SUM(CASE WHEN status = 'refused' THEN 1 ELSE 0 END) AS refused").
		Table("agent_tool_approval").Where("created_unix >= ?", since.Unix()).
		GroupBy("tool").OrderBy("total DESC").Limit(runStatsLimit).Find(&tools); err != nil {
		return nil, fmt.Errorf("add up the tools that were asked for: %w", err)
	}
	for _, row := range tools {
		stats.Tools = append(stats.Tools, RunStatTool{Tool: row.Tool, Total: row.Total, Allowed: row.Allowed, Refused: row.Refused})
	}
	return stats, nil
}

// runStatsLimit is how many models and tools the summary names; the rest is in the runs below.
const runStatsLimit = 8

// SuccessRate is the share of the runs that finished that answered, as a percentage: a run that
// failed or was stopped did not. It is 0 when nothing finished.
func (s *RunStats) SuccessRate() int64 {
	if s.Finished == 0 {
		return 0
	}
	return s.Succeeded * 100 / s.Finished
}

// Count returns how many of a name were seen, or 0.
func (s *RunStats) Count(name string) int64 {
	for _, entry := range s.ByState {
		if entry.Name == name {
			return entry.Count
		}
	}
	return 0
}

// ApprovalCount returns how many approvals ended in the given way, or 0.
func (s *RunStats) ApprovalCount(status string) int64 {
	for _, entry := range s.Approvals {
		if entry.Name == status {
			return entry.Count
		}
	}
	return 0
}

// RunStatsWindow reads the window a page asked for: a number of days, or a moment history starts
// from when it names none.
func RunStatsWindow(days int, now time.Time) time.Time {
	if days <= 0 {
		return time.Unix(0, 0)
	}
	return now.AddDate(0, 0, -days)
}

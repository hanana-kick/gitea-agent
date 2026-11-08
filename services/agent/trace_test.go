// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/unittest"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/services/agent/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What a round handed the model is what the trace reads back: the system prompt, the tools that
// were offered and the messages that were sent, round by round.
func TestPromptTraceRoundTrip(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer func(previous string) { setting.AppDataPath = previous }(setting.AppDataPath)
	setting.AppDataPath = t.TempDir()

	run := &agent_model.Run{ID: 9, ConversationID: 7, UserID: 2}
	conversation := &agent_model.Conversation{ID: 7, Kind: agent_model.ConversationKindRepository}
	messages := []llm.Message{
		{Role: "system", Content: "you answer through a chat transcript"},
		{Role: "user", Content: "read the readme"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "gitea_read_file", Arguments: `{"path":"README.md"}`},
		}}},
	}
	tools := []llm.ToolDef{{Type: "function", Function: llm.ToolFunction{Name: "gitea_read_file", Description: "Read a file."}}}

	writePromptTrace(t.Context(), run, conversation, 0, messages, tools)
	writePromptTrace(t.Context(), run, conversation, 1, messages, nil)

	body, err := ReadPromptTrace(run.ID)
	require.NoError(t, err)
	trace := promptTrace{}
	require.NoError(t, json.Unmarshal(body, &trace))
	assert.Equal(t, 1, trace.Round, "the last round a run reached is the one read back")
	assert.Equal(t, int64(7), trace.ConversationID)
	assert.Equal(t, agent_model.ConversationKindRepository, trace.Kind)
	assert.Equal(t, int64(2), trace.UserID)
	assert.Equal(t, "you answer through a chat transcript", trace.System)
	require.Len(t, trace.Messages, 3)
	assert.Equal(t, "read the readme", trace.Messages[1].Content)
	require.Len(t, trace.Messages[2].ToolCalls, 1)
	assert.Equal(t, "gitea_read_file", trace.Messages[2].ToolCalls[0].Name)
	assert.JSONEq(t, `{"path":"README.md"}`, trace.Messages[2].ToolCalls[0].Arguments)
	assert.Empty(t, trace.Tools, "the last round goes without tools")

	first, err := os.ReadFile(filepath.Join(setting.AppDataPath, "agent-prompt-traces", "7", "9", "round-0.json"))
	require.NoError(t, err)
	opened := promptTrace{}
	require.NoError(t, json.Unmarshal(first, &opened))
	require.Len(t, opened.Tools, 1)
	assert.Equal(t, "gitea_read_file", opened.Tools[0].Name)
	assert.Equal(t, "Read a file.", opened.Tools[0].Description)

	// a run the instance never traced has nothing to read back
	_, err = ReadPromptTrace(4242)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// A run's first round is where its traces are counted and where the ones past the bounds go:
// a tracing instance does not fill its data path.
func TestPromptTraceKeepsTheNewestRuns(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer func(previous string) { setting.AppDataPath = previous }(setting.AppDataPath)
	setting.AppDataPath = t.TempDir()

	root := filepath.Join(setting.AppDataPath, "agent-prompt-traces")
	conversation := &agent_model.Conversation{ID: 7, Kind: agent_model.ConversationKindRepository}
	messages := []llm.Message{{Role: "system", Content: "you answer through a chat transcript"}}
	// one trace per run, oldest first, with the order written into the modification times
	for id := int64(1); id <= int64(traceLimitsDefault.runs); id++ {
		writePromptTrace(t.Context(), &agent_model.Run{ID: id, ConversationID: 7}, conversation, 0, messages, nil)
		at := time.Now().Add(time.Duration(id-int64(traceLimitsDefault.runs)) * time.Minute)
		require.NoError(t, os.Chtimes(filepath.Join(root, "7", strconv.FormatInt(id, 10)), at, at))
	}
	line := func(id int64) string { return strconv.FormatInt(id, 10) }

	// the runs that fit are all there, and one more joins them
	writePromptTrace(t.Context(), &agent_model.Run{ID: int64(traceLimitsDefault.runs) + 1, ConversationID: 7}, conversation, 0, messages, nil)
	for id := int64(1); id <= int64(traceLimitsDefault.runs)+1; id++ {
		_, err := os.Stat(filepath.Join(root, "7", line(id)))
		require.NoError(t, err, "run %d was dropped too early", id)
	}

	// the next run makes room: the oldest goes, the newest stays
	writePromptTrace(t.Context(), &agent_model.Run{ID: int64(traceLimitsDefault.runs) + 2, ConversationID: 7}, conversation, 0, messages, nil)
	_, err := os.Stat(filepath.Join(root, "7", line(1)))
	require.ErrorIs(t, err, os.ErrNotExist)
	for id := int64(2); id <= int64(traceLimitsDefault.runs)+2; id++ {
		_, err := os.Stat(filepath.Join(root, "7", line(id)))
		require.NoError(t, err, "run %d was dropped", id)
	}
	body, err := ReadPromptTrace(int64(traceLimitsDefault.runs) + 2)
	require.NoError(t, err)
	assert.Contains(t, string(body), "you answer through a chat transcript")
}

// The traces of a run go when it is past any of the bounds: too many runs, too old, or more than
// the traces are allowed to take. The newest is the last to go, and a run inside every bound stays.
func TestPruneTracesDropsByCountAgeAndSize(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer func(previous string) { setting.AppDataPath = previous }(setting.AppDataPath)
	setting.AppDataPath = t.TempDir()
	root := filepath.Join(setting.AppDataPath, "agent-prompt-traces")

	plant := func(session string, run int, age time.Duration, size int) string {
		dir := filepath.Join(root, session, strconv.Itoa(run))
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "round-0.json"), bytes.Repeat([]byte("x"), size), 0o600))
		at := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(dir, at, at))
		return dir
	}

	// three runs of 8 bytes each, a moment apart: what the bounds leave is the point
	cases := []struct {
		name        string
		limits      traceLimits
		keepsMiddle bool
		because     string
	}{
		{"count", traceLimits{runs: 2, age: time.Hour, bytes: 1024}, true, "the newest two runs fit"},
		{"age", traceLimits{runs: 8, age: time.Minute, bytes: 1024}, true, "the middle run is inside the age bound"},
		{"size", traceLimits{runs: 8, age: time.Hour, bytes: 12}, false, "the newest run alone reaches the cap"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			require.NoError(t, os.RemoveAll(root))
			newest := plant("7", 1, 0, 8)
			middle := plant("7", 2, 30*time.Second, 8)
			oldest := plant("7", 3, 2*time.Minute, 8)

			pruneTraces(root, testCase.limits)

			_, err := os.Stat(newest)
			require.NoError(t, err, "the newest run was dropped")
			_, err = os.Stat(oldest)
			require.ErrorIs(t, err, os.ErrNotExist, "the oldest run was kept past "+testCase.name)
			_, err = os.Stat(middle)
			if testCase.keepsMiddle {
				require.NoError(t, err, testCase.because)
				return
			}
			require.ErrorIs(t, err, os.ErrNotExist, testCase.because)
		})
	}
}

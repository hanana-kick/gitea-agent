// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/services/agent/llm"
)

// traceLimits bounds what the traces may cost on disk: a debug artifact is dropped by count, by
// age and by total size, whichever comes first. A traced instance therefore has a floor and a
// ceiling it cannot grow past, and the newest run is always the last to go.
type traceLimits struct {
	runs  int
	age   time.Duration
	bytes int64
}

// traceLimitsDefault keeps a week of traces, the newest 200 runs, under about 64 MiB.
var traceLimitsDefault = traceLimits{runs: 200, age: 7 * 24 * time.Hour, bytes: 64 * 1024 * 1024}

// promptTrace is one round of a run as it was handed to the model, whole: what a live run was
// given can be read back rather than guessed at.
type promptTrace struct {
	RunID          int64          `json:"run_id"`
	ConversationID int64          `json:"conversation_id"`
	Kind           string         `json:"kind"`
	UserID         int64          `json:"user_id"`
	Model          string         `json:"model"`
	Round          int            `json:"round"`
	System         string         `json:"system"`
	Tools          []traceTool    `json:"tools"`
	Messages       []traceMessage `json:"messages"`
}

// traceTool is one tool a round offered, as the request named it.
type traceTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// traceMessage is one message of a round.
type traceMessage struct {
	Role       string          `json:"role"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Content    string          `json:"content"`
	ToolCalls  []traceToolCall `json:"tool_calls,omitempty"`
}

// traceToolCall is a call the model made, which the round after it answers.
type traceToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// writePromptTrace stores one round of a run. It never fails the run it traces: a trace that
// cannot be written is a lost diagnostic, not a lost answer.
func writePromptTrace(ctx context.Context, run *agent_model.Run, conversation *agent_model.Conversation,
	round int, messages []llm.Message, tools []llm.ToolDef,
) {
	ref, err := ModelRoleRef(ctx, ModelRoleForKind(conversation.Kind))
	if err != nil {
		log.Error("Agent trace: the model of session %d failed to be read: %v", conversation.ID, err)
	}
	trace := promptTrace{
		RunID: run.ID, ConversationID: conversation.ID, Kind: conversation.Kind,
		UserID: run.UserID, Model: ref, Round: round,
	}
	// index 0 is the system prompt: buildPrompt always writes it first
	if len(messages) > 0 {
		trace.System = messages[0].Content
	}
	for _, tool := range tools {
		trace.Tools = append(trace.Tools, traceTool{Name: tool.Function.Name, Description: tool.Function.Description})
	}
	for _, message := range messages {
		entry := traceMessage{Role: message.Role, ToolCallID: message.ToolCallID, Content: message.Content}
		for _, call := range message.ToolCalls {
			entry.ToolCalls = append(entry.ToolCalls, traceToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
		trace.Messages = append(trace.Messages, entry)
	}
	body, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		log.Error("Agent trace: the request of run %d failed to be encoded: %v", run.ID, err)
		return
	}

	root := filepath.Join(setting.AppDataPath, "agent-prompt-traces")
	// a run's first round is where its directory appears, so that is where the traces beyond the
	// newest traceKeepRuns are dropped
	if round == 0 {
		pruneTraces(root, traceLimitsDefault)
	}

	directory := filepath.Join(root, strconv.FormatInt(conversation.ID, 10), strconv.FormatInt(run.ID, 10))
	if err := os.MkdirAll(directory, 0o750); err != nil {
		log.Error("Agent trace: the directory of run %d failed to be made: %v", run.ID, err)
		return
	}
	if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("round-%d.json", round)), body, 0o600); err != nil {
		log.Error("Agent trace: round %d of run %d failed to be written: %v", round, run.ID, err)
	}
}

// pruneTraces drops the traces of runs that are past what an instance keeps: the oldest go
// first, and a run is kept while it is inside every bound.
func pruneTraces(root string, limits traceLimits) {
	type traceRun struct {
		path string
		mod  time.Time
		size int64
	}
	runs := make([]traceRun, 0, limits.runs+16)
	sessions, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Error("Agent trace: the traces under %s failed to be listed: %v", root, err)
		return
	}
	for _, session := range sessions {
		if !session.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, session.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil || !entry.IsDir() {
				continue
			}
			path := filepath.Join(root, session.Name(), entry.Name())
			runs = append(runs, traceRun{path: path, mod: info.ModTime(), size: directorySize(path)})
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].mod.After(runs[j].mod) })

	kept, bytes, oldest := 0, int64(0), time.Now().Add(-limits.age)
	for _, run := range runs {
		kept++
		bytes += run.size
		if kept <= limits.runs && bytes <= limits.bytes && run.mod.After(oldest) {
			continue
		}
		if err := os.RemoveAll(run.path); err != nil {
			log.Error("Agent trace: dropping the trace of %s failed: %v", run.path, err)
		}
	}
}

// directorySize is what one run's traces take, so the oldest can go by total size.
func directorySize(path string) int64 {
	total := int64(0)
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && !entry.IsDir() {
			total += info.Size()
		}
	}
	return total
}

// ReadPromptTrace returns the last round a run was traced at; os.ErrNotExist when the instance
// did not trace it, which is what the setting being off leaves behind.
func ReadPromptTrace(runID int64) ([]byte, error) {
	root := filepath.Join(setting.AppDataPath, "agent-prompt-traces")
	newest, found := -1, ""
	sessions, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, session := range sessions {
		if !session.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, session.Name(), strconv.FormatInt(runID, 10)))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			round, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "round-"), ".json"))
			if err != nil || round <= newest {
				continue
			}
			newest, found = round, filepath.Join(root, session.Name(), strconv.FormatInt(runID, 10), entry.Name())
		}
	}
	if found == "" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(found)
}

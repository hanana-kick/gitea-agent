// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"
	"gitea.dev/services/agent/llm"

	"github.com/stretchr/testify/require"
)

// The engine talks to whatever endpoint the session's model points at, so a test can stand in
// for the model: it plays one scripted part, and records what it was offered and told, which
// is the only way to see the request an agent run really makes.
type scriptedModel struct {
	t      *testing.T
	url    string
	call   string // the tool call the model asks for, when it is offered tools
	args   string
	answer string // what it says once the call has been answered
	// textCall is a call the model writes into its answer instead of returning it as one, the
	// way a gateway that does not carry calls leaves it
	textCall string

	// chunks are raw stream chunks, for the shapes a test has to write itself: a call split
	// across chunks, two calls at once, a call with no id. alwaysCall keeps calling after it
	// has been answered, which is how the round limit is reached.
	chunks     []map[string]any
	alwaysCall bool
	// requireSignature refuses an answer whose replayed call lost what the gateway signed it
	// with, which is how one family of gateways rejects a follow-up request
	requireSignature bool
	refusals         int

	mu       sync.Mutex
	requests []scriptedRequest
}

// scriptedRequest is what a test needs of one request: the tools it was offered, and the
// messages it was given.
type scriptedRequest struct {
	Tools    []llm.ToolDef `json:"tools"`
	Messages []llm.Message `json:"messages"`
}

func newScriptedModel(t *testing.T, call, args, answer string) *scriptedModel {
	t.Helper()
	model := &scriptedModel{t: t, call: call, args: args, answer: answer}
	server := httptest.NewServer(http.HandlerFunc(model.serve))
	t.Cleanup(server.Close)
	model.url = server.URL
	return model
}

func (m *scriptedModel) serve(w http.ResponseWriter, r *http.Request) {
	body := struct {
		Stream   bool          `json:"stream"`
		Tools    []llm.ToolDef `json:"tools"`
		Messages []llm.Message `json:"messages"`
	}{}
	decoded, err := io.ReadAll(r.Body)
	defer func() { _ = r.Body.Close() }()
	if err != nil || json.Unmarshal(decoded, &body) != nil {
		// a request this test cannot read is a failure of the test, not of the code under it
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.requests = append(m.requests, scriptedRequest{Tools: body.Tools, Messages: body.Messages})
	answered := false
	for _, message := range body.Messages {
		if message.Role == "tool" {
			answered = true
		}
	}
	m.mu.Unlock()

	// the naming call carries no tools and is none of this script's business
	if len(body.Tools) == 0 {
		writeAnswer(w, body.Stream, m.answer)
		return
	}
	if m.requireSignature && answered {
		signed := false
		for _, message := range body.Messages {
			if message.Role != "assistant" {
				continue
			}
			for _, call := range message.ToolCalls {
				if _, ok := call.Extra["extra_content"]; ok {
					signed = true
				}
			}
		}
		if !signed {
			m.mu.Lock()
			m.refusals++
			m.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"the call lost its signature"}}`))
			return
		}
	}
	if len(m.chunks) > 0 {
		if answered && !m.alwaysCall {
			writeAnswer(w, body.Stream, m.answer)
			return
		}
		streamChunks(w, m.chunks...)
		return
	}
	if !answered {
		if m.textCall != "" {
			writeAnswer(w, body.Stream, m.textCall)
			return
		}
		writeCall(w, body.Stream, m.call, m.args)
		return
	}
	writeAnswer(w, body.Stream, m.answer)
}

// requestsMade is a copy of what the model was asked, in order.
func (m *scriptedModel) requestsMade() []scriptedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]scriptedRequest{}, m.requests...)
}

// toolsOffered names the tools one request was offered.
func toolsOffered(request scriptedRequest) []string {
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func writeCall(w http.ResponseWriter, stream bool, name, args string) {
	call := map[string]any{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": name, "arguments": args},
	}
	if !stream {
		writeJSON(w, map[string]any{"id": "scripted", "model": "scripted", "choices": []any{
			map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{call}}, "finish_reason": "tool_calls"},
		}})
		return
	}
	streamChunks(w, map[string]any{"choices": []any{
		map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": nil},
	}})
}

func writeAnswer(w http.ResponseWriter, stream bool, answer string) {
	if !stream {
		writeJSON(w, map[string]any{"id": "scripted", "model": "scripted", "choices": []any{
			map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"},
		}})
		return
	}
	streamChunks(w, map[string]any{"choices": []any{
		map[string]any{"index": 0, "delta": map[string]any{"content": answer}, "finish_reason": nil},
	}})
}

func writeStream(w http.ResponseWriter, chunk map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(chunk) // a test script is always encodable
	fmt.Fprintf(w, "data: %s\n\n", payload)
}

// openStream says that more chunks follow; the caller ends the stream itself.
func openStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
}

// streamChunks writes streamed chunks and ends the stream.
func streamChunks(w http.ResponseWriter, chunks ...map[string]any) {
	openStream(w)
	for _, chunk := range chunks {
		writeStream(w, chunk)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	payload, _ := json.Marshal(body)
	_, _ = w.Write(payload)
}

var engineOnce sync.Once

// startScriptedAgent points the agent's models at one scripted endpoint and returns the
// session a test can drive, with the user whose rights its tools run with.
func startScriptedAgent(t *testing.T, model *scriptedModel, kind string, repoID int64) (*agent_model.Conversation, *user_model.User) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, unittest.PrepareTestDatabase())

	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	key, err := CreateAPIKey(ctx, owner.ID, "scripted-"+t.Name(), "sk-test")
	require.NoError(t, err)
	provider := &agent_model.Provider{
		UserID: owner.ID, Name: "scripted", Provider: "openai", BaseURL: model.url,
		Model: "scripted", APIKeyID: key.ID,
	}
	require.NoError(t, db.Insert(ctx, provider))
	require.NoError(t, SetModelRoleRef(ctx, ModelRoleForKind(kind), fmt.Sprintf("provider:%d/scripted", provider.ID)))

	conversation, err := CreateConversation(ctx, kind, owner.ID, repoID, "scripted session")
	require.NoError(t, err)
	t.Cleanup(func() { _ = CancelRun(conversation.ID) })

	// the tools of a run work as the agent's account, so a test has to name one
	previousAgent := setting.Agent.Username
	setting.Agent.Username = owner.Name
	t.Cleanup(func() { setting.Agent.Username = previousAgent })

	engineOnce.Do(func() { require.NoError(t, InitEngine(ctx)) })
	return conversation, owner
}

// runTurn sends one message and waits for the run it starts to end.
func runTurn(t *testing.T, conversation *agent_model.Conversation, user *user_model.User, message string) *agent_model.Run {
	t.Helper()
	ctx := context.Background()
	_, run, err := SubmitUserMessage(ctx, conversation, user.ID, message)
	require.NoError(t, err)
	require.NotNil(t, run)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		latest, err := LatestRun(ctx, conversation.ID)
		require.NoError(t, err)
		if latest != nil && !agent_model.IsRunActive(latest.Status) {
			return latest
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %d never finished", run.ID)
	return nil
}

// messagesOf lists a session's transcript.
func messagesOf(t *testing.T, conversationID int64) []*agent_model.Message {
	t.Helper()
	list, err := ListMessages(context.Background(), conversationID, 50)
	require.NoError(t, err)
	return list
}

// toolRecord returns the transcript entry a tool left behind, if it ran.
func toolRecord(t *testing.T, conversationID int64, name string) string {
	t.Helper()
	for _, message := range messagesOf(t, conversationID) {
		if message.Role == agent_model.MessageRoleTool && strings.HasPrefix(message.Content, name) {
			return message.Content
		}
	}
	return ""
}

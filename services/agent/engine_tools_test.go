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
	"gitea.dev/services/agent/llm"
	agent_tools "gitea.dev/services/agent/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// the shared model catalog is fetched over the network; a test says so in the only way
	// that costs nothing: an endpoint that refuses at once, leaving every lookup to its default
	llm.ModelCatalogURL = "http://127.0.0.1:9/api.json"
	unittest.MainTest(m)
}

// The engine talks to whatever endpoint the session's model points at, so a test can stand in
// for the model: it plays one scripted part, and records what it was offered and told, which
// is the only way to see the request an agent run really makes.
type scriptedModel struct {
	t      *testing.T
	url    string
	call   string // the tool call the model asks for, when it is offered tools
	args   string
	answer string // what it says once the call has been answered

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
	if !answered {
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
	writeStream(w, map[string]any{"choices": []any{
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
	writeStream(w, map[string]any{"choices": []any{
		map[string]any{"index": 0, "delta": map[string]any{"content": answer}, "finish_reason": nil},
	}})
}

func writeStream(w http.ResponseWriter, chunk map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(chunk) // a test script is always encodable
	fmt.Fprintf(w, "data: %s\n\n", payload)
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

// A session's tools are offered to the model, run for real under the asking user's rights,
// handed back, and the answer that follows is stored.
func TestEngineRunsTheToolsASessionWasOffered(t *testing.T) {
	model := newScriptedModel(t, "gitea_read_file", `{"path":"README.md"}`, "the readme is short")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	requests := model.requestsMade()
	require.Len(t, requests, 2, "one request to call the tool, one to answer with what it read")
	offered := toolsOffered(requests[0])
	assert.Contains(t, offered, "gitea_read_file")
	// a repository session works inside one repository, so the list of them has nothing to say
	assert.NotContains(t, offered, "gitea_list_repos")
	assert.Contains(t, offered, "gitea_read_issue")

	// the tool ran against the repository of the session, not a placeholder
	record := toolRecord(t, conversation.ID, "gitea_read_file")
	require.NotEmpty(t, record)
	assert.Contains(t, record, "`README.md` at master")

	// and the model was handed what it read before it answered
	second := requests[1]
	require.GreaterOrEqual(t, len(second.Messages), 3)
	last := second.Messages[len(second.Messages)-1]
	assert.Equal(t, "tool", last.Role)
	assert.Contains(t, last.Content, "`README.md` at master")

	final := messagesOf(t, conversation.ID)
	assert.Equal(t, "the readme is short", final[len(final)-1].Content)
	assert.Equal(t, agent_model.MessageRoleAssistant, final[len(final)-1].Role)
}

// The tools a session kind may use are the tools it is offered: a session that spans the
// instance is not handed the ones that only make sense inside a repository it is bound to.
func TestEngineOffersTheToolsASessionKindMayUse(t *testing.T) {
	model := newScriptedModel(t, "gitea_list_repos", `{}`, "two repositories")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindServer, 0)

	// a skill this instance has: the tool that opens one is offered only where there is one,
	// and its name reaches the prompt without its steps (the fixtures are loaded afresh per
	// test, so this row has to come after the session does)
	ctx := context.Background()
	require.NoError(t, db.Insert(ctx, &agent_model.Skill{
		Name: "scripted-skill", Scope: agent_model.SkillScopeCommon, Enabled: true,
		Description: "Do the scripted thing", Body: "1. Do it",
	}))
	t.Cleanup(func() { _, _ = db.GetEngine(ctx).Where("name = ?", "scripted-skill").Delete(new(agent_model.Skill)) })

	run := runTurn(t, conversation, user, "what repositories are there")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	requests := model.requestsMade()
	require.NotEmpty(t, requests)
	offered := toolsOffered(requests[0])
	assert.Contains(t, offered, "gitea_list_repos")
	assert.Contains(t, offered, "gitea_read_file") // it names the repository it reads
	assert.Contains(t, offered, "gitea_read_skill")
	// the catalog names the skill, and only its name: the steps wait to be opened
	system := requests[0].Messages[0]
	assert.Equal(t, "system", system.Role)
	assert.Contains(t, system.Content, "scripted-skill")
	assert.NotContains(t, system.Content, "1. Do it")

	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_list_repos"), "user2/repo1")
}

// A tool the admin switched off is neither offered nor runnable: a model that asks for it
// anyway is told so, and the run still ends with an answer.
func TestEngineRefusesAToolThatIsSwitchedOff(t *testing.T) {
	model := newScriptedModel(t, "gitea_read_file", `{"path":"README.md"}`, "I could not read it")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(context.Background(), "gitea_read_file", agent_tools.PermissionLLM, false))
	t.Cleanup(func() {
		_ = agent_tools.SetMode(context.Background(), "gitea_read_file", agent_tools.PermissionLLM, true)
	})

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	requests := model.requestsMade()
	require.NotEmpty(t, requests)
	assert.NotContains(t, toolsOffered(requests[0]), "gitea_read_file")

	record := toolRecord(t, conversation.ID, "gitea_read_file")
	assert.Contains(t, record, "switched off")
}

// A tool that asks first stops the run until its reader answers, and runs once they do.
func TestEngineWaitsForTheApprovalOfAManualTool(t *testing.T) {
	model := newScriptedModel(t, "gitea_read_file", `{"path":"README.md"}`, "the readme is short")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(context.Background(), "gitea_read_file", agent_tools.PermissionManual, true))
	t.Cleanup(func() {
		_ = agent_tools.SetMode(context.Background(), "gitea_read_file", agent_tools.PermissionLLM, true)
	})

	ctx := context.Background()
	_, run, err := SubmitUserMessage(ctx, conversation, user.ID, "read the readme")
	require.NoError(t, err)

	// the run waits for a reader instead of running the call
	var approval *agent_model.ToolApproval
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := PendingApprovals(ctx, conversation.ID)
		require.NoError(t, err)
		if len(pending) > 0 {
			approval = pending[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, approval, "the call was never put to its reader")
	assert.Equal(t, "gitea_read_file", approval.Tool)
	assert.Contains(t, approval.Arguments, "README.md")
	assert.Empty(t, toolRecord(t, conversation.ID, "gitea_read_file"), "the call ran before it was allowed")
	latest, err := LatestRun(ctx, conversation.ID)
	require.NoError(t, err)
	require.True(t, agent_model.IsRunActive(latest.Status), "the run did not wait for the answer")

	require.NoError(t, ResolveApproval(ctx, conversation.ID, approval.ID, user.ID, true))
	for time.Now().Before(deadline) {
		latest, err = LatestRun(ctx, conversation.ID)
		require.NoError(t, err)
		if !agent_model.IsRunActive(latest.Status) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Equal(t, agent_model.RunStatusSucceeded, latest.Status, latest.Error)
	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_read_file"), "`README.md` at master")

	answered, err := PendingApprovals(ctx, conversation.ID)
	require.NoError(t, err)
	assert.Empty(t, answered, "an answered call is no longer pending")

	stored := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(approval.ID).Get(stored)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalAllowed, stored.Status)
	assert.Equal(t, user.ID, stored.DecidedUserID)
	_ = run
}

// A pull request of the session's repository is read as the change it is: the review path
// every other tool was built for.
func TestEngineReadsAPullRequestChange(t *testing.T) {
	model := newScriptedModel(t, "gitea_read_pull_request", `{"index":3}`, "it adds a file")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "what does pull request 3 change")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	record := toolRecord(t, conversation.ID, "gitea_read_pull_request")
	require.NotEmpty(t, record)
	assert.Contains(t, record, "files changed, +")
}

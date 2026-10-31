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

// A call arrives the way a gateway really sends one: the name once, the arguments in pieces.
// What the tool finally runs with is the assembled call, not the first piece.
func TestEngineAssemblesACallThatArrivesInPieces(t *testing.T) {
	model := newScriptedModel(t, "", "", "the readme is short")
	model.chunks = streamedCall([]map[string]any{
		{"name": "gitea_read_file", "arguments": `{"pa`},
		{"arguments": `th":"REA`},
		{"arguments": `DME.md"}`},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	record := toolRecord(t, conversation.ID, "gitea_read_file")
	require.NotEmpty(t, record)
	assert.Contains(t, record, `{"path":"README.md"}`)
	assert.Contains(t, record, "`README.md` at master")
}

// Two calls in one message are both run, and both answers name the call they answer.
func TestEngineAnswersEveryCallOfARound(t *testing.T) {
	model := newScriptedModel(t, "", "", "both were read")
	model.chunks = streamedCall([]map[string]any{
		{"index": 0, "id": "call_7", "name": "gitea_read_file", "arguments": `{"path":"README.md"}`},
		{"index": 1, "id": "call_8", "name": "gitea_read_issue", "arguments": `{"index":1}`},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read the readme and issue 1")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_read_file"), "`README.md` at master")
	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_read_issue"), "user2/repo1 #1")

	// the model is told which answer belongs to which call, and the call it asked for is
	// replayed exactly as it was made
	requests := model.requestsMade()
	require.Len(t, requests, 2)
	replayed := requests[1].Messages
	var assistantCalls []string
	results := map[string]string{}
	for _, message := range replayed {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				assistantCalls = append(assistantCalls, call.ID)
			}
		}
		if message.Role == "tool" {
			results[message.ToolCallID] = message.Content
		}
	}
	assert.Equal(t, []string{"call_7", "call_8"}, assistantCalls)
	assert.Contains(t, results, "call_7")
	assert.Contains(t, results, "call_8")
	assert.Contains(t, results["call_7"], "`README.md` at master")
}

// A gateway that leaves a call unnamed still gets an answer the provider can line up.
func TestEngineNamesACallTheProviderLeftUnnamed(t *testing.T) {
	model := newScriptedModel(t, "", "", "read it")
	model.chunks = streamedCall([]map[string]any{
		{"name": "gitea_read_file", "arguments": `{"path":"README.md"}`},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	requests := model.requestsMade()
	require.Len(t, requests, 2)
	for _, message := range requests[1].Messages {
		if message.Role != "tool" {
			continue
		}
		assert.NotEmpty(t, message.ToolCallID, "an answer with no call id cannot be lined up")
		assert.NotEmpty(t, message.Content)
	}
}

// A model that only ever calls tools is stopped at the round limit instead of running forever,
// and its reader is told why.
func TestEngineStopsAModelThatOnlyCallsTools(t *testing.T) {
	model := newScriptedModel(t, "", "", "I could not settle on an answer")
	model.alwaysCall = true
	model.chunks = streamedCall([]map[string]any{
		{"name": "gitea_read_file", "arguments": `{"path":"README.md"}`},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "keep reading")
	// the last round is asked without tools, so a model with nothing to say still ends the run
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	assert.Equal(t, "I could not settle on an answer", messagesOf(t, conversation.ID)[len(messagesOf(t, conversation.ID))-1].Content)

	// and it does not call forever: the tools are asked for a bounded number of times
	assert.LessOrEqual(t, len(model.requestsMade()), engineToolRounds)
	reads := 0
	for _, message := range messagesOf(t, conversation.ID) {
		if message.Role == agent_model.MessageRoleTool {
			reads++
		}
	}
	assert.Equal(t, engineToolRounds-1, reads, "one round goes without tools, so it cannot call")
}

// streamedCall writes one message's worth of calls as a gateway streams them: each piece
// carries what it knows, and the pieces of one call share its index.
func streamedCall(calls []map[string]any) []map[string]any {
	chunks := make([]map[string]any, 0, len(calls)+1)
	for _, call := range calls {
		index := 0 // the pieces of one call share its index; separate calls carry their own
		if given, ok := call["index"].(int); ok {
			index = given
		}
		fn := map[string]any{"arguments": call["arguments"]}
		if name, ok := call["name"]; ok {
			fn["name"] = name
		}
		piece := map[string]any{"index": index, "type": "function", "function": fn}
		for key, value := range call {
			switch key {
			case "index", "name", "arguments", "function":
			default:
				piece[key] = value // whatever else a gateway signs a call with
			}
		}
		chunks = append(chunks, map[string]any{"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{piece}}, "finish_reason": nil},
		}})
	}
	return chunks
}

// A gateway that sends the arguments as an object rather than as text still gets its call run:
// the call is worth more than the shape it arrived in.
func TestEngineRunsACallWhoseArgumentsCameAsAnObject(t *testing.T) {
	model := newScriptedModel(t, "", "", "read it")
	model.chunks = streamedCall([]map[string]any{
		{"name": "gitea_read_file", "arguments": map[string]any{"path": "README.md"}},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	record := toolRecord(t, conversation.ID, "gitea_read_file")
	require.NotEmpty(t, record)
	assert.Contains(t, record, "`README.md` at master")
}

// A tool that takes nothing is called with no arguments, and a model that makes up a tool is
// told so instead of the run dying on it.
func TestEngineAnswersCallsWithNoArgumentsAndCallsToNothing(t *testing.T) {
	model := newScriptedModel(t, "", "", "nothing and nowhere")
	model.chunks = streamedCall([]map[string]any{
		{"index": 0, "id": "call_1", "name": "gitea_search_issues", "arguments": ""},
		{"index": 1, "id": "call_2", "name": "gitea_nonesuch", "arguments": `{"anything":true}`},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "look around")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	// the tool that takes nothing ran
	searched := toolRecord(t, conversation.ID, "gitea_search_issues")
	require.NotEmpty(t, searched)
	assert.NotContains(t, searched, "could not run")

	// the made-up one is answered, and the run carries on to an answer
	madeUp := toolRecord(t, conversation.ID, "gitea_nonesuch")
	require.NotEmpty(t, madeUp)
	assert.Contains(t, madeUp, "unknown tool")

	final := messagesOf(t, conversation.ID)
	assert.Equal(t, "nothing and nowhere", final[len(final)-1].Content)
}

// A gateway that signs a call and refuses the answer without the signature gets it back: what
// the gateway sent with a call is kept and replayed with it.
func TestEngineReplaysWhatAGatewaySignedACallWith(t *testing.T) {
	model := newScriptedModel(t, "", "", "the readme is short")
	model.requireSignature = true
	model.chunks = streamedCall([]map[string]any{
		{
			"name": "gitea_read_file", "arguments": `{"path":"README.md"}`,
			"extra_content": map[string]any{"google": map[string]any{"thought_signature": "sig-1"}},
		},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	assert.Zero(t, model.refusals, "the answered request arrived without the signature")
	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_read_file"), "`README.md` at master")
}

// Two calls that a gateway named alike are told apart, so the two answers are not confused
// for each other.
func TestEngineTellsApartCallsTheGatewayNamedAlike(t *testing.T) {
	model := newScriptedModel(t, "", "", "both were read")
	model.chunks = streamedCall([]map[string]any{
		{"index": 0, "id": "call_1", "name": "gitea_read_file", "arguments": `{"path":"README.md"}`},
		{"index": 1, "id": "call_1", "name": "gitea_read_issue", "arguments": `{"index":1}`},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read both")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	requests := model.requestsMade()
	require.Len(t, requests, 2)
	ids := []string{}
	answered := []string{}
	for _, message := range requests[1].Messages {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				ids = append(ids, call.ID)
			}
		}
		if message.Role == "tool" {
			answered = append(answered, message.ToolCallID)
		}
	}
	require.Len(t, ids, 2)
	assert.NotEqual(t, ids[0], ids[1], "two calls cannot share one name back")
	assert.ElementsMatch(t, ids, answered, "every call is answered by its own name")
}

// The model can put a document in the arguments of a call; the reader of a record does not have
// to be given all of it.
func TestEngineCapsTheArgumentsItRecords(t *testing.T) {
	model := newScriptedModel(t, "", "", "listed")
	model.chunks = streamedCall([]map[string]any{
		{"name": "gitea_list_files", "arguments": `{"path":"","note":"` + strings.Repeat("x", 8*1024) + `"}`},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "list the files")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	record := toolRecord(t, conversation.ID, "gitea_list_files")
	require.NotEmpty(t, record)
	line, _, _ := strings.Cut(record, "\n")
	assert.LessOrEqual(t, len(line), engineArgsLimit+len("gitea_list_files ")+4)
	assert.Contains(t, line, "…")
	assert.Contains(t, record, "README.md", "the tool still ran with the arguments it was given")
}

// A call that names no tool at all is answered in words the model can act on.
func TestEngineAnswersACallThatNamesNoTool(t *testing.T) {
	model := newScriptedModel(t, "", "", "nothing to call")
	model.chunks = streamedCall([]map[string]any{{"arguments": `{}`}})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "call something")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	var told string
	for _, message := range messagesOf(t, conversation.ID) {
		if message.Role == agent_model.MessageRoleTool {
			told = message.Content
		}
	}
	assert.Contains(t, told, "named no tool")
}

// A gateway that never says which call a piece belongs to: one call, read as one.
func TestEngineReadsACallThatNeverSaysItsIndex(t *testing.T) {
	model := newScriptedModel(t, "", "", "read it")
	model.chunks = []map[string]any{
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "gitea_read_file"}},
		}}, "finish_reason": nil}}},
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"type": "function", "function": map[string]any{"arguments": `{"path":"README.md"}`}},
		}}, "finish_reason": nil}}},
	}
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_read_file"), "`README.md` at master")
}

// A gateway that puts two different calls on one index is making two calls, and both run.
func TestEngineReadsTwoCallsThatShareAnIndex(t *testing.T) {
	model := newScriptedModel(t, "", "", "both were read")
	model.chunks = streamedCall([]map[string]any{
		{"index": 0, "id": "call_a", "name": "gitea_read_file", "arguments": `{"path":"README.md"}`},
		{"index": 0, "id": "call_b", "name": "gitea_read_issue", "arguments": `{"index":1}`},
	})
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read both")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_read_file"), "`README.md` at master")
	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_read_issue"), "user2/repo1 #1")

	requests := model.requestsMade()
	require.Len(t, requests, 2)
	answered := []string{}
	for _, message := range requests[1].Messages {
		if message.Role == "tool" {
			answered = append(answered, message.ToolCallID)
		}
	}
	assert.ElementsMatch(t, []string{"call_a", "call_b"}, answered)
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"gitea.dev/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageMarshal(t *testing.T) {
	text, err := json.Marshal(ChatRequest{Messages: []Message{{Role: "user", Content: "hello"}}})
	require.NoError(t, err)
	assert.Contains(t, string(text), `"content":"hello"`)

	picture, err := json.Marshal(Message{Role: "user", Content: "what is this", Images: []string{"data:image/png;base64,AAA"}})
	require.NoError(t, err)
	var parts struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(picture, &parts))
	require.Len(t, parts.Content, 2)
	assert.Equal(t, "user", parts.Role)
	assert.Equal(t, "text", parts.Content[0].Type)
	assert.Equal(t, "what is this", parts.Content[0].Text)
	assert.Equal(t, "image_url", parts.Content[1].Type)
	assert.Equal(t, "data:image/png;base64,AAA", parts.Content[1].ImageURL.URL)

	// a picture without any words still has to carry its role
	promptOnly, err := json.Marshal(Message{Role: "user", Images: []string{"data:image/png;base64,AAA"}})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(promptOnly, &parts))
	require.Len(t, parts.Content, 1)
	assert.Equal(t, "image_url", parts.Content[0].Type)

	tool, err := json.Marshal(Message{Role: "tool", Content: "3 repos", ToolCallID: "call_1"})
	require.NoError(t, err)
	assert.Contains(t, string(tool), `"tool_call_id":"call_1"`)
	assert.Contains(t, string(tool), `"content":"3 repos"`)

	calls, err := json.Marshal(Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "gitea_list_repos", Arguments: "{}"}}}})
	require.NoError(t, err)
	assert.Contains(t, string(calls), `"name":"gitea_list_repos"`)

	// tools ride along on the request only when there are any
	asked, err := json.Marshal(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []ToolDef{{Type: "function", Function: ToolFunction{Name: "gitea_list_repos"}}}})
	require.NoError(t, err)
	assert.Contains(t, string(asked), `"tools":[{"type":"function","function":{"name":"gitea_list_repos"`)
	quiet, err := json.Marshal(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	require.NoError(t, err)
	assert.NotContains(t, string(quiet), "tools")
}

// streamClient answers with a canned event stream, whatever the request was.
type streamClient struct{ body string }

func (c *streamClient) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(c.body)),
	}, nil
}

func TestDoChatStreamAssemblesToolCalls(t *testing.T) {
	stream := &streamClient{body: strings.Join([]string{
		`data: {"id":"c1","model":"gpt","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"gitea_search_issues","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"keyword\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"bug\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"gitea_list_repos"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")}

	var pieces []string
	resp, err := DoChatStream(context.Background(), stream, "https://example.test/chat", nil, ChatRequest{Model: "gpt"}, func(delta Delta) error {
		pieces = append(pieces, delta.Content)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)

	// a call arrives in pieces: the name once and the arguments in fragments
	assert.Equal(t, []ToolCall{
		{ID: "call_1", Type: "function", Function: FunctionCall{Name: "gitea_search_issues", Arguments: `{"keyword":"bug"}`}},
		{ID: "call_2", Type: "function", Function: FunctionCall{Name: "gitea_list_repos"}},
	}, resp.Choices[0].Message.ToolCalls)
	assert.Empty(t, strings.Join(pieces, ""), "a call-only answer carries no text")
}

func TestDoChatStreamKeepsTextAndCalls(t *testing.T) {
	stream := &streamClient{body: strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"looking it up"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"gitea_list_repos","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
	}, "\n")}

	var pieces []string
	resp, err := DoChatStream(context.Background(), stream, "https://example.test/chat", nil, ChatRequest{Model: "gpt"}, func(delta Delta) error {
		pieces = append(pieces, delta.Content)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "looking it up", resp.Choices[0].Message.Content)
	require.Len(t, resp.Choices[0].Message.ToolCalls, 1)
	assert.Equal(t, "gitea_list_repos", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	assert.Equal(t, []string{"looking it up"}, pieces)
}

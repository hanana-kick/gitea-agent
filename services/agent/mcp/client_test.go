// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubServer is a Model Context Protocol server of the test's own: it answers initialize, lists
// two tools and calls one of them, the way a server of the streamable HTTP transport does.
type stubServer struct {
	t        *testing.T
	asked    []string
	streamed bool // answer with a stream instead of a document, which a server may do
	wants    string
}

func (s *stubServer) start() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(s.t, "application/json", r.Header.Get("Content-Type"))
		assert.Contains(s.t, r.Header.Get("Accept"), "text/event-stream", "a client must accept both kinds of answer")
		if s.wants != "" {
			assert.Equal(s.t, "Bearer "+s.wants, r.Header.Get("Authorization"))
		}
		raw, err := io.ReadAll(r.Body)
		if !assert.NoError(s.t, err) {
			return
		}
		var request struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if !assert.NoError(s.t, json.Unmarshal(raw, &request)) {
			return
		}
		s.asked = append(s.asked, request.Method)
		w.Header().Set("Content-Type", "application/json")

		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-1")
			s.write(w, request.ID, map[string]any{
				"protocolVersion": ProtocolVersion,
				"serverInfo":      map[string]any{"name": "stub", "version": "1"},
			})
		case "notifications/initialized":
			assert.Equal(s.t, "session-1", r.Header.Get("Mcp-Session-Id"), "a session id must be carried on")
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			assert.Equal(s.t, ProtocolVersion, r.Header.Get("MCP-Protocol-Version"))
			s.write(w, request.ID, map[string]any{"tools": []map[string]any{
				{
					"name": "echo", "title": "Echo", "description": "Answers what it is given",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				},
				{"name": "fail", "description": "Always fails"},
			}})
		case "tools/call":
			// the call's arguments are nested under the request's params, as the protocol has them
			var call struct {
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			if !assert.NoError(s.t, json.Unmarshal(raw, &call)) {
				return
			}
			params := call.Params
			if params.Name == "unknown" {
				s.writeError(w, request.ID, -32602, "no tool named unknown")
				return
			}
			if params.Name == "fail" {
				s.write(w, request.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "no"}}, "isError": true})
				return
			}
			text, _ := params.Arguments["text"].(string)
			s.write(w, request.ID, map[string]any{"content": []map[string]any{
				{"type": "text", "text": "echo: " + text},
			}})
		default:
			s.writeError(w, request.ID, -32601, "no such method")
		}
	}))
	return server
}

// write answers with a document, or with a stream carrying it.
func (s *stubServer) write(w http.ResponseWriter, id int64, result any) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if !assert.NoError(s.t, err) {
		return
	}
	if !s.streamed {
		_, _ = w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("event: message\ndata: " + string(body) + "\n\n"))
}

func (s *stubServer) writeError(w http.ResponseWriter, id int64, code int, message string) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	if !assert.NoError(s.t, err) {
		return
	}
	_, _ = w.Write(body)
}

// A server is introduced once, its tools are read, and one of them is called: what the server
// says about a tool is what the model is told, and what it answers is what the call returns.
func TestClientSpeaksToAServer(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		stub := &stubServer{t: t, streamed: streamed, wants: "a-secret"}
		server := stub.start()
		defer server.Close()

		client := NewClient(server.URL, "a-secret")
		require.NoError(t, client.Initialize(context.Background()))
		assert.Equal(t, "session-1", client.sessionID)

		tools, err := client.ListTools(context.Background())
		require.NoError(t, err)
		require.Len(t, tools, 2)
		assert.Equal(t, "echo", tools[0].Name)
		assert.Equal(t, "Echo", tools[0].Title)
		assert.Contains(t, string(tools[0].InputSchema), `"text"`)

		answer, err := client.CallTool(context.Background(), "echo", `{"text":"hello"}`)
		require.NoError(t, err)
		assert.Equal(t, "echo: hello", answer)

		// a tool that failed is a failure here, not an answer
		_, err = client.CallTool(context.Background(), "fail", `{}`)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no")

		assert.Equal(t, []string{"initialize", "notifications/initialized", "tools/list", "tools/call", "tools/call"}, stub.asked)
	}
}

// A server that answers an error is telling the caller what went wrong, not failing silently.
func TestClientReportsWhatAServerSaid(t *testing.T) {
	stub := &stubServer{t: t}
	server := stub.start()
	defer server.Close()

	client := NewClient(server.URL, "")
	require.NoError(t, client.Initialize(context.Background()))
	_, err := client.CallTool(context.Background(), "unknown", `{}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no tool named unknown")
}

// Arguments that are not an object are refused before anything is sent.
func TestClientRefusesArgumentsThatAreNotAnObject(t *testing.T) {
	client := NewClient("http://127.0.0.1:9/mcp", "")
	_, err := client.CallTool(context.Background(), "echo", `"not an object"`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an object")
}

// A server that is not there is reported as a transport failure.
func TestClientReportsAnUnreachableServer(t *testing.T) {
	client := NewClient("http://127.0.0.1:9/mcp", "")
	err := client.Initialize(context.Background())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "connect") || strings.Contains(err.Error(), "refused"), err.Error())
}

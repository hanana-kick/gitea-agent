// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package mcp talks to Model Context Protocol servers, so the tools a server offers join the ones
// the agent may call. It speaks the streamable HTTP transport: JSON-RPC documents posted to one
// endpoint, with the session a server hands out kept for the calls that follow.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// ProtocolVersion is the revision of the protocol this client speaks.
	ProtocolVersion = "2025-06-18"
	// clientName is how this client introduces itself.
	clientName = "gitea-agent"
	// requestLimit is how long one call to a server may take.
	requestLimit = 30 * time.Second
	// resultLimit is how much of a server's answer is read: a tool's answer is text a model reads.
	resultLimit = 64 * 1024
)

// ErrServer is what a server itself said went wrong, rather than the transport failing.
type ErrServer struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (err ErrServer) Error() string {
	return fmt.Sprintf("the server answered an error (%d): %s", err.Code, err.Message)
}

// Client is one conversation with a server: a server may hand out a session id when it is
// initialized, and every request after that carries it.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client

	mu          sync.Mutex
	sessionID   string
	initialized bool
	nextID      int64
}

// Tool is one tool a server offers, as the server described it: the schema is passed on to the
// model exactly as it came, because only the server knows what its tool takes.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// NewClient prepares a client for one server, with the bearer token it wants when it wants one.
func NewClient(url, token string) *Client {
	return &Client{URL: strings.TrimSpace(url), Token: token, HTTP: &http.Client{Timeout: requestLimit}}
}

// Initialize introduces this client and reads what the server answered. It is called once before
// anything else, and a session id in the answer is kept for the calls that follow.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": "1"},
	}
	result := struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}{}
	if err := c.call(ctx, "initialize", params, &result); err != nil {
		return err
	}
	c.mu.Lock()
	c.initialized = true
	c.mu.Unlock()
	// a notification is answered with no body, and a server that refuses it is not asked again
	return c.notify(ctx, "notifications/initialized", map[string]any{})
}

// ListTools reads every tool the server offers, following the pages it hands out.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	tools := make([]Tool, 0, 16)
	cursor := ""
	// a server that keeps handing out cursors is not followed forever
	for range 20 {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result := struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}{}
		if err := c.call(ctx, "tools/list", params, &result); err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return tools, nil
}

// CallTool calls one tool of the server and returns what it answered as text. A tool that says it
// failed reads as a failure here too, so a caller does not have to know the protocol.
func (c *Client) CallTool(ctx context.Context, name, arguments string) (string, error) {
	args := map[string]any{}
	if trimmed := strings.TrimSpace(arguments); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return "", fmt.Errorf("the arguments are not an object: %w", err)
		}
	}
	result := struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}{}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return "", err
	}
	text := make([]string, 0, len(result.Content))
	for _, part := range result.Content {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			text = append(text, part.Text)
		}
	}
	answer := strings.Join(text, "\n")
	if strings.TrimSpace(answer) == "" {
		answer = "The tool answered nothing."
	}
	if result.IsError {
		return "", errors.New(answer)
	}
	return answer, nil
}

// call posts one request and reads the answer, whether the server answered with a document or
// with a stream that carries it.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	status, contentType, raw, sessionID, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if sessionID != "" {
		c.sessionID = sessionID
	}
	c.mu.Unlock()
	if status == http.StatusNotFound && c.sessionID != "" {
		// the server forgot the session: one more try, introduced again
		c.mu.Lock()
		c.sessionID, c.initialized = "", false
		c.mu.Unlock()
		return fmt.Errorf("the server no longer knows this session")
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("the server answered HTTP %d: %s", status, firstLine(raw))
	}
	document, err := messageOf(contentType, raw, id)
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *ErrServer      `json:"error"`
	}
	if err := json.Unmarshal(document, &envelope); err != nil {
		return fmt.Errorf("the server answered something that is not a JSON-RPC document: %w", err)
	}
	if envelope.Error != nil {
		return *envelope.Error
	}
	if out == nil {
		return nil
	}
	if len(envelope.Result) == 0 {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

// notify posts a notification, which a server answers with no body at all.
func (c *Client) notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	status, _, raw, _, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted && (status < 200 || status > 299) {
		return fmt.Errorf("the server answered HTTP %d to %s: %s", status, method, firstLine(raw))
	}
	return nil
}

// post sends one document to the server and reads what came back, capped.
func (c *Client) post(ctx context.Context, body []byte) (status int, contentType string, raw []byte, sessionID string, err error) {
	ctx, cancel := context.WithTimeout(ctx, requestLimit)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", nil, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	// both are accepted: a server may answer with a document or with a stream carrying it
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	c.mu.Lock()
	session := c.sessionID
	c.mu.Unlock()
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: requestLimit}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", nil, "", err
	}
	defer response.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(response.Body, resultLimit))
	if err != nil {
		return response.StatusCode, "", nil, "", err
	}
	return response.StatusCode, response.Header.Get("Content-Type"), raw, response.Header.Get("Mcp-Session-Id"), nil
}

// messageOf reads the JSON-RPC document out of an answer: a server may answer with the document
// itself or with a stream whose events carry it.
func messageOf(contentType string, raw []byte, id int64) ([]byte, error) {
	if !strings.Contains(contentType, "text/event-stream") {
		return raw, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), resultLimit)
	fallback := []byte(nil)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		event := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		if len(event) == 0 {
			continue
		}
		var envelope struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(event, &envelope); err != nil {
			continue
		}
		if envelope.ID == id {
			return event, nil
		}
		if fallback == nil {
			fallback = event
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, errors.New("the stream carried no answer")
}

// firstLine is the beginning of an answer, for an error a reader can act on.
func firstLine(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

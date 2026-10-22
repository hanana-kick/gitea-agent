// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/json"
)

// Message represents a chat message.
type Message struct {
	Role    string `json:"role"` // system, user, assistant, tool
	Content string `json:"content"`
	// Images are data URLs of pictures sent with the message. Only models that accept
	// them get them; the router drops them for the others.
	Images []string `json:"-"`
	// ToolCalls is what the model asked to run. Only assistant messages carry it.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID names the call a tool message answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// MarshalJSON writes the OpenAI shape: content is a bare string for text and a list of
// parts once the message carries pictures.
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Images) == 0 {
		type plain Message
		return json.Marshal(plain(m))
	}
	parts := make([]map[string]any, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, image := range m.Images {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": image}})
	}
	body := map[string]any{"role": m.Role, "content": parts}
	if m.ToolCallID != "" {
		body["tool_call_id"] = m.ToolCallID
	}
	return json.Marshal(body)
}

// ToolCall is one call the model asked the agent to make.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool and carries its arguments as JSON.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef describes one tool the model may call.
type ToolDef struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the callable half of a ToolDef; Parameters is a JSON schema.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatRequest is the request body for chat completion.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
	// Tools are the calls the model may ask for; empty means the model only answers.
	Tools []ToolDef `json:"tools,omitempty"`
	// ToolChoice narrows those calls; empty leaves the decision to the model.
	ToolChoice string `json:"tool_choice,omitempty"`
}

// WithoutImages returns the messages with their pictures dropped, for a model that
// reads text only.
func WithoutImages(messages []Message) []Message {
	dropped := make([]Message, 0, len(messages))
	for _, message := range messages {
		message.Images = nil
		dropped = append(dropped, message)
	}
	return dropped
}

// HasImages reports whether any message carries a picture.
func HasImages(messages []Message) bool {
	for _, message := range messages {
		if len(message.Images) > 0 {
			return true
		}
	}
	return false
}

// ChatResponse is the response body for chat completion.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
}

// Choice represents a single completion choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ModelArchitecture describes how a model takes and returns data. Only some
// providers publish it; the field stays empty elsewhere.
type ModelArchitecture struct {
	InputModalities []string `json:"input_modalities"`
}

// ModelInfo is a model offered by a provider.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Architecture is nil when the provider does not describe its models.
	Architecture *ModelArchitecture `json:"architecture,omitempty"`
}

// InputModalities returns the inputs the model accepts, or nil when unknown.
func (m ModelInfo) InputModalities() []string {
	if m.Architecture == nil {
		return nil
	}
	modalities := make([]string, 0, len(m.Architecture.InputModalities))
	for _, modality := range m.Architecture.InputModalities {
		if modality = strings.TrimSpace(modality); modality != "" {
			modalities = append(modalities, modality)
		}
	}
	slices.Sort(modalities)
	return modalities
}

// Delta is one piece of a streamed answer. Reasoning carries what the model thought
// before answering, which only some models send.
type Delta struct {
	Content   string
	Reasoning string
	// Reset tells the receiver to drop what it has shown so far; a fallback member
	// starts over after the one before it failed.
	Reset bool
}

// OnDelta receives the pieces of a streamed answer.
type OnDelta func(Delta) error

// Client is the interface for LLM providers.
type Client interface {
	// Chat sends a chat completion request and returns the assistant's reply.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// ChatStream sends the same request but reports the answer as it arrives.
	ChatStream(ctx context.Context, req ChatRequest, onDelta OnDelta) (ChatResponse, error)
	// ListModels returns the models the provider exposes.
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// DoListModels fetches and parses a model listing endpoint. Both the OpenAI
// envelope ({"data": [...]}) and a bare array are accepted.
func DoListModels(ctx context.Context, httpClient HTTPClient, url string, header http.Header) ([]ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	maps.Copy(httpReq.Header, header)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("LLM API error: " + string(respBody))
	}

	var envelope struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(respBody, &envelope); err == nil && envelope.Data != nil {
		return cleanModels(envelope.Data), nil
	}
	var bare []ModelInfo
	if err := json.Unmarshal(respBody, &bare); err != nil {
		return nil, err
	}
	return cleanModels(bare), nil
}

func cleanModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	seen := map[string]bool{}
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelInfo{ID: id, Name: strings.TrimSpace(m.Name), Architecture: m.Architecture})
	}
	return out
}

const (
	// streamIdleTimeout is how long a stream may stay silent before it counts as dead.
	streamIdleTimeout = 2 * time.Minute
	streamIdleCheck   = 5 * time.Second
)

// DoChatStream sends a streaming chat request and calls onDelta for every piece it reads.
// The returned response carries the assembled answer.
func DoChatStream(ctx context.Context, httpClient HTTPClient, url string, header http.Header, req ChatRequest, onDelta OnDelta) (ChatResponse, error) {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	maps.Copy(httpReq.Header, header)

	// a stream that goes silent is dead: providers send at least keep-alives while working
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var idleMu sync.Mutex
	lastPiece := time.Now()
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		ticker := time.NewTicker(streamIdleCheck)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-ticker.C:
				idleMu.Lock()
				idle := time.Since(lastPiece)
				idleMu.Unlock()
				if idle > streamIdleTimeout {
					cancelStream()
					return
				}
			}
		}
	}()
	httpReq = httpReq.WithContext(streamCtx)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ChatResponse{}, errors.New("LLM API error: " + string(respBody))
	}

	// some gateways ignore stream:true and answer with a whole completion
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return ChatResponse{}, err
		}
		var whole ChatResponse
		if err := json.Unmarshal(respBody, &whole); err != nil {
			return ChatResponse{}, err
		}
		if onDelta != nil && len(whole.Choices) > 0 {
			if err := onDelta(Delta{Content: whole.Choices[0].Message.Content}); err != nil {
				return ChatResponse{}, err
			}
		}
		return whole, nil
	}

	var answer, reasoning strings.Builder
	var id, model string
	var toolCalls []ToolCall
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 32*1024), 1024*1024)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data:")
		if !ok {
			continue // blank separators, comments and "event:" lines
		}
		if payload = strings.TrimSpace(payload); payload == "[DONE]" {
			break
		}
		var chunk struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // a malformed keep-alive must not kill the answer
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return ChatResponse{}, errors.New("LLM API error: " + chunk.Error.Message)
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		idleMu.Lock()
		lastPiece = time.Now()
		idleMu.Unlock()

		// some providers do not send [DONE], the finish reason is the end of the answer
		finished := chunk.Choices[0].FinishReason != ""
		delta := chunk.Choices[0].Delta
		// a call arrives in pieces: the name once and the arguments in fragments
		for _, call := range delta.ToolCalls {
			for len(toolCalls) <= call.Index {
				toolCalls = append(toolCalls, ToolCall{Type: "function"})
			}
			piece := &toolCalls[call.Index]
			if call.ID != "" {
				piece.ID = call.ID
			}
			if piece.Function.Name == "" {
				piece.Function.Name = call.Function.Name
			}
			piece.Function.Arguments += call.Function.Arguments
		}
		thought := delta.Reasoning + delta.ReasoningContent
		if delta.Content == "" && thought == "" {
			if finished {
				break
			}
			continue // a call fragment carries nothing to show yet
		}
		answer.WriteString(delta.Content)
		reasoning.WriteString(thought)
		if onDelta != nil {
			if err := onDelta(Delta{Content: delta.Content, Reasoning: thought}); err != nil {
				return ChatResponse{}, err
			}
		}
		if finished {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		if streamCtx.Err() != nil && ctx.Err() == nil {
			return ChatResponse{}, errors.New("the model stopped sending its answer")
		}
		return ChatResponse{}, err
	}

	return ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Model:   model,
		Choices: []Choice{{Index: 0, Message: Message{Role: "assistant", Content: answer.String(), ToolCalls: toolCalls}}},
	}, nil
}

// HTTPClient is an interface for making HTTP requests (wraps *http.Client).
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// normalizeBaseURL returns baseURL without a trailing slash, or def when baseURL is empty.
func normalizeBaseURL(baseURL, def string) string {
	if baseURL == "" {
		return def
	}
	return strings.TrimSuffix(baseURL, "/")
}

// headersFromMap converts custom headers into an http.Header, skipping empty names.
func headersFromMap(headers map[string]string) http.Header {
	h := http.Header{}
	for k, v := range headers {
		if k == "" {
			continue
		}
		h.Set(k, v)
	}
	return h
}

// DoChat sends a chat request to the given endpoint with the given headers and returns the parsed response.
func DoChat(ctx context.Context, httpClient HTTPClient, method, url string, header http.Header, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	maps.Copy(httpReq.Header, header)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, errors.New("LLM API error: " + string(respBody))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return ChatResponse{}, err
	}
	return chatResp, nil
}

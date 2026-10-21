// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	llm "gitea.dev/services/agent/llm"
)

// maxRoleDepth bounds how deep roles may reference each other.
const maxRoleDepth = 16

// RouteAttempt records one member that was tried.
type RouteAttempt struct {
	Role   string // role the attempt came from
	Target string // "provider/model" or the name of a nested role
	Error  string // empty when the attempt answered
}

// RouteResult is the outcome of routing a request through a group.
type RouteResult struct {
	Role     string
	Model    string
	Reply    string
	Attempts []RouteAttempt
}

// TestGroup routes a minimal chat request through a group to prove the wiring works.
func TestGroup(ctx context.Context, roleID int64) (*RouteResult, error) {
	role, err := GetModelGroupByID(ctx, roleID)
	if err != nil {
		return nil, err
	}
	return ChatThroughGroup(ctx, role, llm.ChatRequest{
		Messages:  []llm.Message{{Role: "user", Content: "ping"}},
		MaxTokens: 5,
	})
}

// ChatThroughGroup sends a request to one of the group's members, honouring its call mode.
func ChatThroughGroup(ctx context.Context, role *agent_model.ModelGroup, req llm.ChatRequest) (*RouteResult, error) {
	return chatThroughGroup(ctx, role, req, nil)
}

// ChatThroughGroupStream does the same but reports the answer as it arrives.
func ChatThroughGroupStream(ctx context.Context, role *agent_model.ModelGroup, req llm.ChatRequest, onDelta llm.OnDelta) (*RouteResult, error) {
	return chatThroughGroup(ctx, role, req, onDelta)
}

func chatThroughGroup(ctx context.Context, role *agent_model.ModelGroup, req llm.ChatRequest, onDelta llm.OnDelta) (*RouteResult, error) {
	result := &RouteResult{Role: role.Name}
	if err := routeRequest(ctx, role, req, result, 0, onDelta); err != nil {
		return result, err
	}
	return result, nil
}

// routeRequest tries the members of a group; the first answer wins.
func routeRequest(ctx context.Context, role *agent_model.ModelGroup, req llm.ChatRequest, result *RouteResult, depth int, onDelta llm.OnDelta) error {
	if depth > maxRoleDepth {
		return errors.New("role nesting is too deep")
	}
	members, err := agent_model.FindGroupMembers(ctx, role.ID)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return errors.New("role has no members")
	}

	// round robin starts at the cursor and remembers where it left off
	start := 0
	if role.ModeOrDefault() == agent_model.GroupModeRoundRobin {
		start = role.Cursor % len(members)
	}

	var lastErr error
	for offset := range members {
		member := members[(start+offset)%len(members)]
		// a member that fails mid answer leaves streamed text behind, so the next one starts clean
		produced := false
		var memberDelta llm.OnDelta
		if onDelta != nil {
			memberDelta = func(delta llm.Delta) error {
				produced = true
				return onDelta(delta)
			}
		}
		lastErr = tryMember(ctx, role, member, req, result, depth, memberDelta)
		if lastErr == nil {
			if role.ModeOrDefault() == agent_model.GroupModeRoundRobin {
				// the next call starts at the following member
				role.Cursor = (start + offset + 1) % len(members)
				if _, err := db.GetEngine(ctx).ID(role.ID).Cols("cursor").Update(role); err != nil {
					return err
				}
			}
			return nil
		}
		if produced {
			if err := onDelta(llm.Delta{Reset: true}); err != nil {
				return err
			}
		}
	}
	return lastErr
}

// tryMember calls a single member, recursing when the member is another group.
func tryMember(ctx context.Context, role *agent_model.ModelGroup, member *agent_model.ModelGroupMember, req llm.ChatRequest, result *RouteResult, depth int, onDelta llm.OnDelta) error {
	if member.MemberGroupID != 0 {
		nested, err := GetModelGroupByID(ctx, member.MemberGroupID)
		if err != nil {
			result.Attempts = append(result.Attempts, RouteAttempt{Role: role.Name, Target: fmt.Sprintf("role#%d", member.MemberGroupID), Error: err.Error()})
			return err
		}
		target := nested.Name
		if err := routeRequest(ctx, nested, req, result, depth+1, onDelta); err != nil {
			result.Attempts = append(result.Attempts, RouteAttempt{Role: role.Name, Target: target, Error: err.Error()})
			return err
		}
		// the nested role already recorded its own attempts
		return nil
	}

	provider, err := GetProviderByID(ctx, member.ProviderID)
	if err != nil {
		result.Attempts = append(result.Attempts, RouteAttempt{Role: role.Name, Target: fmt.Sprintf("provider#%d", member.ProviderID), Error: err.Error()})
		return err
	}

	target := provider.Name + "/" + member.Model
	apiKey := ResolveProviderAPIKey(ctx, provider)
	if apiKey == "" {
		err := errors.New("no api key available")
		result.Attempts = append(result.Attempts, RouteAttempt{Role: role.Name, Target: target, Error: err.Error()})
		return err
	}
	client, err := llm.NewClient(provider.Provider, apiKey, provider.BaseURL, member.Model, provider.CustomHeaders())
	if err != nil {
		result.Attempts = append(result.Attempts, RouteAttempt{Role: role.Name, Target: target, Error: err.Error()})
		return err
	}

	chatReq := req
	chatReq.Model = member.Model
	var resp llm.ChatResponse
	if onDelta != nil {
		resp, err = client.ChatStream(ctx, chatReq, onDelta)
	} else {
		resp, err = client.Chat(ctx, chatReq)
	}
	if err != nil {
		result.Attempts = append(result.Attempts, RouteAttempt{Role: role.Name, Target: target, Error: err.Error()})
		return err
	}

	reply, ok := usableReply(resp)
	if !ok {
		// an empty answer is as good as a failure: the next member must try
		err := errors.New("the model returned an empty answer")
		result.Attempts = append(result.Attempts, RouteAttempt{Role: role.Name, Target: target, Error: err.Error()})
		return err
	}

	result.Attempts = append(result.Attempts, RouteAttempt{Role: role.Name, Target: target})
	result.Model = member.Model
	if resp.Model != "" {
		result.Model = resp.Model
	}
	result.Reply = reply
	return nil
}

// usableReply reports whether a response carries an answer worth showing.
func usableReply(resp llm.ChatResponse) (string, bool) {
	if len(resp.Choices) == 0 {
		return "", false
	}
	reply := strings.TrimSpace(resp.Choices[0].Message.Content)
	return reply, reply != ""
}

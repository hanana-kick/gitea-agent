// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	llm "gitea.dev/services/agent/llm"
)

// The two ways a session can name its model: a group (routed) or one provider model.
const (
	modelRefGroup    = "group"
	modelRefProvider = "provider"
)

// ModelOption is one selectable answer target of a session.
type ModelOption struct {
	Ref   string
	Label string
}

// ModelRefForGroup names a group as a session model.
func ModelRefForGroup(slug string) string {
	return modelRefGroup + ":" + slug
}

// ModelRefForProvider names a single provider model as a session model.
func ModelRefForProvider(providerID int64, model string) string {
	return fmt.Sprintf("%s:%d/%s", modelRefProvider, providerID, model)
}

// ErrModelRefInvalid is returned when a session model reference can not be used.
type ErrModelRefInvalid struct{}

func (ErrModelRefInvalid) Error() string { return "the selected model is not available" }

// Is reports whether target is an ErrModelRefInvalid.
func (ErrModelRefInvalid) Is(target error) bool {
	_, ok := target.(ErrModelRefInvalid)
	return ok
}

// ModelOptions lists what a session may answer with: groups first, then provider models.
func ModelOptions(ctx context.Context, limit int) ([]ModelOption, error) {
	groups, _, err := FindModelGroups(ctx, agent_model.FindModelGroupsOptions{
		ListOptions: db.ListOptions{Page: 1, PageSize: limit},
	})
	if err != nil {
		return nil, err
	}
	options := make([]ModelOption, 0, len(groups))
	for _, group := range groups {
		options = append(options, ModelOption{Ref: ModelRefForGroup(group.Slug), Label: group.Name})
	}

	providers, err := GetProviders(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, provider := range providers {
		for _, model := range provider.Models() {
			options = append(options, ModelOption{
				Ref:   ModelRefForProvider(provider.ID, model.ID),
				Label: provider.Name + " / " + model.DisplayName(),
			})
		}
	}
	return options, nil
}

// CanUseModelRef reports whether ref names a model that exists.
func CanUseModelRef(ctx context.Context, ref string) (bool, error) {
	options, err := ModelOptions(ctx, 200)
	if err != nil {
		return false, err
	}
	for _, option := range options {
		if option.Ref == ref {
			return true, nil
		}
	}
	return false, nil
}

// Answer sends a request through the session model reference and returns the reply and the
// model that produced it.
func Answer(ctx context.Context, ref string, req llm.ChatRequest) (reply, model string, err error) {
	kind, rest, ok := strings.Cut(ref, ":")
	if !ok {
		return "", "", ErrModelRefInvalid{}
	}
	switch kind {
	case modelRefGroup:
		group, err := GetModelGroupBySlug(ctx, rest)
		if err != nil {
			return "", "", err
		}
		if group == nil {
			return "", "", ErrModelRefInvalid{}
		}
		result, err := ChatThroughGroup(ctx, group, req)
		if err != nil {
			return "", "", err
		}
		return result.Reply, result.Model, nil
	case modelRefProvider:
		providerID, modelName, ok := strings.Cut(rest, "/")
		if !ok || modelName == "" {
			return "", "", ErrModelRefInvalid{}
		}
		id, err := strconv.ParseInt(providerID, 10, 64)
		if err != nil {
			return "", "", ErrModelRefInvalid{}
		}
		provider, err := GetProviderByID(ctx, id)
		if err != nil {
			return "", "", err
		}
		if provider == nil {
			return "", "", ErrModelRefInvalid{}
		}
		apiKey := ResolveProviderAPIKey(ctx, provider)
		if apiKey == "" {
			return "", "", errors.New("no api key available")
		}
		client, err := llm.NewClient(provider.Provider, apiKey, provider.BaseURL, modelName, provider.CustomHeaders())
		if err != nil {
			return "", "", err
		}
		req.Model = modelName
		resp, err := client.Chat(ctx, req)
		if err != nil {
			return "", "", err
		}
		answered := modelName
		if resp.Model != "" {
			answered = resp.Model
		}
		if len(resp.Choices) == 0 {
			return "", answered, errors.New("the model returned no answer")
		}
		return resp.Choices[0].Message.Content, answered, nil
	}
	return "", "", ErrModelRefInvalid{}
}

// ModelRefLabel renders a reference for humans, falling back to the raw value.
func ModelRefLabel(ctx context.Context, ref string) string {
	if ref == "" {
		return ""
	}
	options, err := ModelOptions(ctx, 200)
	if err != nil {
		return ref
	}
	for _, option := range options {
		if option.Ref == ref {
			return option.Label
		}
	}
	return ref
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	llm "gitea.dev/services/agent/llm"
)

// scopePrompt describes what the session is allowed to look after. It is written for the
// model, not for the UI, so it stays in English.
func scopePrompt(ctx context.Context, conversation *agent_model.Conversation) string {
	switch conversation.Kind {
	case agent_model.ConversationKindResident:
		return "You are Pod, the resident agent of this Gitea instance. You look after the whole " +
			"instance and keep your context between conversations."
	case agent_model.ConversationKindServer:
		return "You are Pod, working in a throwaway server session of this Gitea instance. You have " +
			"the same reach as the resident Pod agent, but your context is kept apart from it and is " +
			"meant to be discarded when this session ends."
	case agent_model.ConversationKindRepository:
		name := ""
		if repo, err := repo_model.GetRepositoryByID(ctx, conversation.RepoID); err == nil && repo != nil {
			name = repo.FullName()
		}
		return "You are Pod, working in a session bound to the repository " + name + ". You are " +
			"expected to work on that repository: code changes, pull requests and CI."
	}
	return "You are Pod, an agent of this Gitea instance."
}

// buildPrompt renders the messages handed to the model: the scope prompt, then the transcript.
func buildPrompt(ctx context.Context, conversation *agent_model.Conversation, history []*agent_model.Message) []llm.Message {
	messages := make([]llm.Message, 0, len(history)+1)
	messages = append(messages, llm.Message{
		Role: "system",
		Content: scopePrompt(ctx, conversation) + "\n\n" +
			"You answer through a chat transcript. Tool use is not available yet, so answer from what " +
			"you know and say plainly when something can not be done. Answer in the language the user " +
			"writes in, and keep answers short unless asked for detail.",
	})

	// keep the newest history when the transcript is long
	recent := history
	if len(recent) > engineHistoryLimit {
		recent = recent[len(recent)-engineHistoryLimit:]
	}
	total := 0
	kept := make([]llm.Message, 0, len(recent))
	for i := len(recent) - 1; i >= 0; i-- {
		message := recent[i]
		if message.Role == agent_model.MessageRoleSystem {
			continue
		}
		total += len(message.Content)
		if total > enginePromptLimit && len(kept) > 0 {
			break
		}
		kept = append(kept, llm.Message{Role: message.Role, Content: message.Content})
	}
	for i := len(kept) - 1; i >= 0; i-- {
		messages = append(messages, kept[i])
	}
	return messages
}

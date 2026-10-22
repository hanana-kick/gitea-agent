// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"slices"
	"strings"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	llm "gitea.dev/services/agent/llm"
	agent_tools "gitea.dev/services/agent/tools"
)

// messageFiles renders the files sent with a message: text files are inlined where they
// can be read, pictures become data URLs the model is shown when it accepts them.
func messageFiles(message *agent_model.Message, attachments []*agent_model.Attachment) (string, []string) {
	if len(attachments) == 0 {
		return message.Content, nil
	}
	builder := strings.Builder{}
	builder.WriteString(message.Content)
	var images []string
	for _, attachment := range attachments {
		if picture := ImageDataURL(attachment); picture != "" {
			builder.WriteString("\n\n[attached image: " + attachment.Name + " (" + attachment.SizeText() + ")]")
			images = append(images, picture)
			continue
		}
		text := AttachmentText(attachment)
		if text == "" {
			builder.WriteString("\n\n[attached file: " + attachment.Name + " (" + attachment.SizeText() + ")]")
			continue
		}
		builder.WriteString("\n\n[attached file: " + attachment.Name + "]\n```\n" + text + "\n```")
	}
	return builder.String(), images
}

// toolsPrompt tells the model what it may look up for itself. It is written for the
// model, so it stays in English.
func toolsPrompt(offerings []agent_tools.Tool) string {
	if len(offerings) == 0 {
		return "You answer from what you know and say plainly when something can not be done."
	}
	builder := strings.Builder{}
	builder.WriteString("You can call the tools below to read this Gitea instance yourself. Call one ")
	builder.WriteString("whenever it beats guessing, and say what you looked up; never invent what a ")
	builder.WriteString("tool would have returned.\n")
	for _, tool := range offerings {
		builder.WriteString("- " + tool.Name() + ": " + tool.Description() + "\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

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

// buildPrompt renders the messages handed to the model: the scope prompt, then the
// transcript. Files sent with a message are inlined when they are text and attached as
// pictures when the model accepts them. Tool records are history for the reader, so they
// stay out of the prompt.
func buildPrompt(ctx context.Context, conversation *agent_model.Conversation, history []*agent_model.Message, attachments map[int64][]*agent_model.Attachment, offerings []agent_tools.Tool) []llm.Message {
	messages := make([]llm.Message, 0, len(history)+1)
	messages = append(messages, llm.Message{
		Role: "system",
		Content: scopePrompt(ctx, conversation) + "\n\n" +
			"You answer through a chat transcript. Answer in the language the user writes in, " +
			"and keep answers short unless asked for detail.\n\n" +
			toolsPrompt(offerings),
	})

	// keep the newest history when the transcript is long; the summary stands in for
	// everything dropped, so older context is not lost entirely
	recent := history
	if len(recent) > engineHistoryLimit {
		recent = recent[len(recent)-engineHistoryLimit:]
	}
	total := 0
	kept := make([]llm.Message, 0, len(recent))
	for i := range slices.Backward(recent) {
		message := recent[i]
		if message.Role != agent_model.MessageRoleUser && message.Role != agent_model.MessageRoleAssistant {
			continue
		}
		content, images := messageFiles(message, attachments[message.ID])
		total += len(content)
		for _, picture := range images {
			total += len(picture)
		}
		if total > enginePromptLimit && len(kept) > 0 {
			break
		}
		kept = append(kept, llm.Message{Role: message.Role, Content: content, Images: images})
	}
	if summary := strings.TrimSpace(conversation.Summary); summary != "" {
		messages = append(messages, llm.Message{Role: "system", Content: "Earlier in this conversation: " + summary})
	}
	for i := range slices.Backward(kept) {
		messages = append(messages, kept[i])
	}
	return messages
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"regexp"
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
	if !slices.ContainsFunc(offerings, agent_tools.Writes) {
		// a session that can only read must say so rather than promise a change it cannot make
		builder.WriteString("\nNothing here changes anything: you can read and report, and when you are asked ")
		builder.WriteString("to change, commit, open or merge something, say that you cannot and what a ")
		builder.WriteString("reader has to switch on for you to do it.\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// skillCatalogPrompt lists the skills of a session by name and one line each. The steps
// themselves stay out of the prompt until the model opens a skill, so the catalog costs
// the prompt a line per skill. It is written for the model, so it stays in English.
func skillCatalogPrompt(list []*agent_model.Skill) string {
	if len(list) == 0 {
		return ""
	}
	builder := strings.Builder{}
	builder.WriteString("You have skills: procedures written down for this session. Open one with ")
	builder.WriteString("gitea_read_skill before you start a task it covers, and follow it.\n")
	shown := 0
	for _, skill := range list {
		line := "- " + skill.Name + ": " + clipLine(skill.Description, skillLineLimit) + "\n"
		if shown == skillCatalogLimit || builder.Len()+len(line) > skillCatalogBytes {
			builder.WriteString("- (more skills are not listed here)\n")
			break
		}
		builder.WriteString(line)
		shown++
	}
	return strings.TrimRight(builder.String(), "\n")
}

// clipLine cuts one line of catalog text to what the prompt can afford.
func clipLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	return strings.TrimSpace(text[:limit]) + "…"
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

// A task is written "/skill @subject" and then whatever the reader wants. The two are
// separate, so either may lead and either may stand alone; the words around them are the
// reader's own.
var (
	// a task leads its message: a line that opens with a token is a task, a path mentioned
	// inside a sentence is not
	agentCommandLead    = regexp.MustCompile(`^\s*[/@][^\s]`)
	agentCommandSkill   = regexp.MustCompile(`(?:^|\s)/([a-z0-9][a-z0-9_-]*)`)
	agentCommandSubject = regexp.MustCompile(`(?:^|\s)@([^\s]+)`)
	agentCommandToken   = regexp.MustCompile(`(?:^|\s)[/@][^\s]+`)
)

// expandCommand reads a task the reader ran and says what the model needs to know about it:
// the skill that runs it, and the subject it is aimed at. A message that carries neither is
// handed over as the reader wrote it. A tool is only named when the session was offered it,
// so a task never sends the model after something it cannot call.
func expandCommand(content string, catalog []*agent_model.Skill, offered map[string]bool) string {
	command := agentCommandSkill.FindStringSubmatch(content)
	subject := agentCommandSubject.FindStringSubmatch(content)
	if command == nil || !agentCommandLead.MatchString(content) {
		return content
	}

	hints := make([]string, 0, 3)
	if offered[agent_tools.SkillToolName] {
		for _, skill := range catalog {
			if skill.Name == command[1] {
				hints = append(hints, "Open the \""+command[1]+"\" skill with "+agent_tools.SkillToolName+" and follow its steps before you answer.")
				break
			}
		}
	}
	if subject != nil {
		if repo, number, ok := strings.Cut(subject[1], "#"); ok {
			hints = append(hints, "The subject is #"+number+" of "+repo+" (an issue, or a pull request when the repository reads it as one):")
			look := make([]string, 0, 3)
			if offered["gitea_read_issue"] {
				look = append(look, "read it with the gitea_read_issue tool before you answer")
			}
			if offered["gitea_read_pull_request"] {
				look = append(look, "when it is a pull request, read what it changes with gitea_read_pull_request")
			}
			if offered["gitea_search_issues"] {
				look = append(look, "search around it with gitea_search_issues")
			}
			if len(look) > 0 {
				hints = append(hints, strings.Join(look, ", and ")+".")
			}
		} else {
			hints = append(hints, "The subject is the repository "+subject[1]+".")
			if offered["gitea_search_issues"] {
				hints = append(hints, "Start from its issues with gitea_search_issues.")
			}
		}
	}

	builder := strings.Builder{}
	builder.WriteString(content)
	builder.WriteString("\n\n[task] The reader ran the command /" + command[1])
	if subject != nil {
		builder.WriteString(" on @" + subject[1])
	}
	builder.WriteString(".")
	if task := strings.TrimSpace(agentCommandToken.ReplaceAllString(content, " ")); task != "" {
		builder.WriteString(" What they asked for: " + task)
	}
	if len(hints) > 0 {
		builder.WriteString("\n" + strings.Join(hints, "\n"))
	}
	return builder.String()
}

// buildPrompt renders the messages handed to the model: the scope prompt, then the
// transcript. Files sent with a message are inlined when they are text and attached as
// pictures when the model accepts them. Tool records are history for the reader, so they
// stay out of the prompt.
func buildPrompt(ctx context.Context, conversation *agent_model.Conversation, history []*agent_model.Message, attachments map[int64][]*agent_model.Attachment, offerings []agent_tools.Tool, catalog []*agent_model.Skill) []llm.Message {
	messages := make([]llm.Message, 0, len(history)+1)
	system := scopePrompt(ctx, conversation) + "\n\n" +
		"You answer through a chat transcript. Answer in the language the user writes in, " +
		"and keep answers short unless asked for detail.\n\n" +
		toolsPrompt(offerings)
	if skills := skillCatalogPrompt(catalog); skills != "" {
		system += "\n\n" + skills
	}
	messages = append(messages, llm.Message{Role: "system", Content: system})

	// keep the newest history when the transcript is long; the summary stands in for
	// everything dropped, so older context is not lost entirely
	recent := history
	if len(recent) > engineHistoryLimit {
		recent = recent[len(recent)-engineHistoryLimit:]
	}
	// a task only names the tools this session was offered
	offered := make(map[string]bool, len(offerings))
	for _, tool := range offerings {
		offered[tool.Name()] = true
	}
	// the system prompt is spent before any history, so the transcript budget accounts for it
	total := len(system)
	kept := make([]llm.Message, 0, len(recent))
	for i := range slices.Backward(recent) {
		message := recent[i]
		if message.Role != agent_model.MessageRoleUser && message.Role != agent_model.MessageRoleAssistant {
			continue
		}
		content, images := messageFiles(message, attachments[message.ID])
		if message.Role == agent_model.MessageRoleUser {
			content = expandCommand(content, catalog, offered) // a task says which skill runs it
		}
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

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agent_model "gitea.dev/models/agent"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/setting"
	llm "gitea.dev/services/agent/llm"
	agent_tools "gitea.dev/services/agent/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTool is a tool with nothing behind it but a description.
type stubTool struct{}

func (stubTool) Name() string        { return "gitea_list_repos" }
func (stubTool) Description() string { return "List the repositories you can see" }
func (stubTool) Scope() agent_tools.Scope {
	return agent_tools.ScopeUser
}
func (stubTool) Args() []agent_tools.Arg { return nil }
func (stubTool) Run(context.Context, *user_model.User, agent_tools.Session, string) (string, error) {
	return "", nil
}

// storeFile puts the bytes of an attachment where AttachmentPath looks.
func storeFile(t *testing.T, attachment *agent_model.Attachment, content string) {
	t.Helper()
	path := AttachmentPath(attachment)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o640))
}

func rolesOf(messages []llm.Message) []string {
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.Role)
	}
	return roles
}

func joined(messages []llm.Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, message.Content)
	}
	return strings.Join(parts, "\n")
}

func TestBuildPromptKeepsTheConversationOnly(t *testing.T) {
	conversation := &agent_model.Conversation{ID: 1, Kind: agent_model.ConversationKindResident, Summary: "Looked at CI"}
	history := []*agent_model.Message{
		{ID: 1, Role: agent_model.MessageRoleUser, Content: "first question"},
		{ID: 2, Role: agent_model.MessageRoleSystem, Content: "an internal note"},
		{ID: 3, Role: agent_model.MessageRoleTool, Content: "gitea_list_repos\nrepo-a"},
		{ID: 4, Role: agent_model.MessageRoleAssistant, Content: "one repository"},
		{ID: 5, Role: agent_model.MessageRoleUser, Content: "and now"},
	}

	prompt := buildPrompt(context.Background(), conversation, history, nil, nil, nil)
	// the scope prompt, then the summary standing in for the older transcript, then the exchange
	require.Len(t, prompt, 5)
	assert.Equal(t, []string{"system", "system", "user", "assistant", "user"}, rolesOf(prompt))
	assert.Contains(t, prompt[0].Content, "resident agent")
	assert.NotContains(t, prompt[0].Content, "call the tools below", "no tools means no tool instructions")
	assert.Contains(t, prompt[1].Content, "Earlier in this conversation: Looked at CI")
	assert.Equal(t, "and now", prompt[4].Content)

	// tool records and internal notes are history for the reader, not for the model
	assert.NotContains(t, joined(prompt), "an internal note")
	assert.NotContains(t, joined(prompt), "gitea_list_repos")
}

func TestBuildPromptNamesTheToolsTheModelMayCall(t *testing.T) {
	conversation := &agent_model.Conversation{ID: 1, Kind: agent_model.ConversationKindServer}

	prompt := buildPrompt(context.Background(), conversation, nil, nil, []agent_tools.Tool{stubTool{}}, nil)
	require.NotEmpty(t, prompt)
	assert.Contains(t, prompt[0].Content, "call the tools below")
	assert.Contains(t, prompt[0].Content, "gitea_list_repos: List the repositories you can see")
}

func TestBuildPromptHandsTheFilesOver(t *testing.T) {
	setting.AppDataPath = t.TempDir()
	conversation := &agent_model.Conversation{ID: 1, Kind: agent_model.ConversationKindResident}
	note := &agent_model.Attachment{ID: 1, ConversationID: 1, MessageID: 1, Name: "notes.txt", Size: 11}
	storeFile(t, note, "hello agent")
	picture := &agent_model.Attachment{ID: 2, ConversationID: 1, MessageID: 1, Name: "red.png", Size: 8}
	storeFile(t, picture, "\x89PNG\r\n\x1a\n")
	tooBig := &agent_model.Attachment{ID: 3, ConversationID: 1, MessageID: 1, Name: "huge.png", Size: 4*1024*1024 + 1}

	history := []*agent_model.Message{{ID: 1, Role: agent_model.MessageRoleUser, Content: "take a look"}}
	attachments := map[int64][]*agent_model.Attachment{1: {note, picture, tooBig}}

	prompt := buildPrompt(context.Background(), conversation, history, attachments, nil, nil)
	require.Len(t, prompt, 2)
	asked := prompt[1]
	assert.Contains(t, asked.Content, "take a look")
	// text the model can read goes inline
	assert.Contains(t, asked.Content, "[attached file: notes.txt]")
	assert.Contains(t, asked.Content, "hello agent")
	// a picture becomes a data URL, with a line saying what it is
	assert.Contains(t, asked.Content, "[attached image: red.png (8 B)]")
	require.Len(t, asked.Images, 1)
	assert.True(t, strings.HasPrefix(asked.Images[0], "data:image/png;base64,"), asked.Images[0])
	// a picture too big to send stays a download for the reader
	assert.Contains(t, asked.Content, "[attached file: huge.png (4.0 MB)]")
}

// skillCatalogPrompt is what a session pays for its skills: a line each, and the steps only
// when the model opens one.
func TestSkillCatalogStaysSmall(t *testing.T) {
	assert.Empty(t, skillCatalogPrompt(nil), "no skills, nothing in the prompt")

	one := skillCatalogPrompt([]*agent_model.Skill{{Name: "issue-triage", Description: "Sort  incoming\nissues"}})
	assert.Contains(t, one, "gitea_read_skill")
	assert.Contains(t, one, "- issue-triage: Sort incoming issues", "the description is one line")
	assert.NotContains(t, one, "steps", "the body is not in the catalog")

	long := strings.Repeat("x", skillLineLimit*2)
	clipped := skillCatalogPrompt([]*agent_model.Skill{{Name: "long", Description: long}})
	assert.Contains(t, clipped, "…")
	assert.Less(t, len(clipped), skillLineLimit+200)

	many := make([]*agent_model.Skill, 0, skillCatalogLimit+5)
	for range skillCatalogLimit + 5 {
		many = append(many, &agent_model.Skill{Name: "skill", Description: "does something"})
	}
	capped := skillCatalogPrompt(many)
	assert.Contains(t, capped, "(more skills are not listed here)")
	assert.LessOrEqual(t, len(capped), skillCatalogBytes+200, "the catalog never grows past its budget")
}

func TestImageDataURLOnlyForPictures(t *testing.T) {
	setting.AppDataPath = t.TempDir()
	note := &agent_model.Attachment{ID: 4, ConversationID: 1, MessageID: 1, Name: "notes.txt", Size: 3}
	storeFile(t, note, "abc")
	assert.Empty(t, ImageDataURL(note), "a text file is not a picture")

	missing := &agent_model.Attachment{ID: 5, ConversationID: 1, MessageID: 1, Name: "gone.png", Size: 3}
	assert.Empty(t, ImageDataURL(missing), "a file that is not there cannot be shown")

	jpeg := &agent_model.Attachment{ID: 6, ConversationID: 1, MessageID: 1, Name: "PHOTO.JPG", Size: 3}
	storeFile(t, jpeg, "abc")
	assert.True(t, strings.HasPrefix(ImageDataURL(jpeg), "data:image/jpeg;base64,"), "the extension is case insensitive")
}

// The reader's tasks arrive as commands: the model has to be told which skill runs one and
// which subject it reads, whichever of the two leads, and an ordinary message is left alone.
func TestExpandCommandNamesTheSkillAndTheSubject(t *testing.T) {
	catalog := []*agent_model.Skill{{Name: "issue-triage", Description: "Sort issues"}, {Name: "code-review", Description: "Read code"}}
	all := map[string]bool{"gitea_read_skill": true, "gitea_read_issue": true, "gitea_search_issues": true}

	task := expandCommand("/issue-triage @acme/widgets#7 look into this", catalog, all)
	assert.Contains(t, task, "The reader ran the command /issue-triage on @acme/widgets#7.")
	assert.Contains(t, task, "What they asked for: look into this")
	assert.Contains(t, task, `Open the "issue-triage" skill with gitea_read_skill`)
	assert.Contains(t, task, "The subject is #7 of acme/widgets")
	assert.Contains(t, task, "gitea_read_issue")

	// the two are separate: the subject may lead, and it may stand alone
	reversed := expandCommand("@acme/widgets#7 /issue-triage look into this", catalog, all)
	assert.Contains(t, reversed, "The subject is #7 of acme/widgets")
	assert.Contains(t, reversed, `Open the "issue-triage" skill`)

	// a subject without a number is the repository itself, and a command without a skill is
	// still a task, so only the skill line is missing
	repoOnly := expandCommand("/code-review @acme/widgets", catalog, all)
	assert.NotContains(t, repoOnly, "gitea_read_issue")
	assert.Contains(t, repoOnly, "The subject is the repository acme/widgets")
	assert.Contains(t, repoOnly, `Open the "code-review" skill`)

	plain := expandCommand("/code @acme/widgets review it", catalog, all)
	assert.NotContains(t, plain, "Open the")
	assert.Contains(t, plain, "The subject is the repository acme/widgets")

	// anything without a skill is not a command: it is handed over as it was written
	for _, message := range []string{"what changed today?", "what about /usr/bin?", "hello @acme/widgets", "and/or"} {
		assert.Equal(t, message, expandCommand(message, catalog, all))
	}

	// the task still reads the same way without any tool to name: a session is never sent
	// after a tool it was not offered
	none := expandCommand("/issue-triage @acme/widgets#7 look into this", catalog, map[string]bool{})
	assert.Contains(t, none, "The reader ran the command /issue-triage on @acme/widgets#7.")
	assert.Contains(t, none, "The subject is #7 of acme/widgets")
	assert.NotContains(t, none, "gitea_")

	repoNoTools := expandCommand("/code @acme/widgets review it", catalog, map[string]bool{})
	assert.Contains(t, repoNoTools, "The subject is the repository acme/widgets.")
	assert.NotContains(t, repoNoTools, "gitea_search_issues")
	// one of the two lookups is offered: only that one is named
	partial := expandCommand("/issue-triage @acme/widgets#7 look into this", catalog, map[string]bool{"gitea_search_issues": true})
	assert.Contains(t, partial, "search around it with gitea_search_issues")
	assert.NotContains(t, partial, "gitea_read_issue")
	assert.NotContains(t, partial, "Open the")
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	issues_model "gitea.dev/models/issues"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/git/gitrepo"
	"gitea.dev/modules/setting"
	agent_tools "gitea.dev/services/agent/tools"
	notify_service "gitea.dev/services/notify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A session is offered the tools its permissions leave on: each mode reaches the model alike,
// and the one tool the admin switched off does not reach it at all. This is the check that a
// run which "had no tools" fails, whatever the model then says about them.
func TestHarnessOffersTheToolsThePermissionsAllow(t *testing.T) {
	ctx := context.Background()
	model := newScriptedModel(t, "gitea_read_file", `{"path":"README.md"}`, "the readme is short")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_read_file", agent_tools.PermissionLLM, true))
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_list_files", agent_tools.PermissionManual, true))
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_commit_files", agent_tools.PermissionAllowAll, true))
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_read_issue", agent_tools.PermissionLLM, false))

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	requests := model.requestsMade()
	require.NotEmpty(t, requests)
	offered := toolsOffered(requests[0])
	assert.Subset(t, offered, []string{"gitea_read_file", "gitea_list_files", "gitea_commit_files"})
	assert.NotContains(t, offered, "gitea_read_issue", "a tool the admin switched off was offered")
}

// A gateway that does not carry calls leaves the model writing them into its answer. Read as the
// answer, the work would never happen and a reader would be shown the syntax of the call format
// in a comment; the call runs, and only the words around it are what anyone is told.
func TestHarnessReadsACallTheModelWroteAsText(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	model := newScriptedModel(t, "", "", "the branch is there now")
	model.textCall = "The page needs a branch of its own, so I will make one.\n" +
		"<tool_call>\n<function=gitea_create_branch>\n" +
		"<parameter=branch>\npage-preview\n</parameter>\n" +
		"<parameter=from_branch>\nmain\n</parameter>\n" +
		"<parameter=repository>\nuser2/repo1\n</parameter>\n" +
		"</function>\n</tool_call>"
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(context.Background(), "gitea_create_branch", agent_tools.PermissionAllowAll, true))

	run := runTurn(t, conversation, user, "publish the page from its own branch")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	// the call ran: the tool it named is in the transcript with what it answered
	record := toolRecord(t, conversation.ID, "gitea_create_branch")
	require.NotEmpty(t, record, "the call the model wrote as text never ran")
	assert.Contains(t, record, "page-preview")
	// and the model was asked again with what the call returned
	requests := model.requestsMade()
	require.Len(t, requests, 2)
	assert.Contains(t, requests[1].Messages[len(requests[1].Messages)-1].Content, "page-preview")

	// nothing anyone reads shows the call format: not the answer, not the transcript
	final := messagesOf(t, conversation.ID)
	require.NotEmpty(t, final)
	assert.Equal(t, "the branch is there now", final[len(final)-1].Content)
	for _, message := range final {
		assert.NotContains(t, message.Content, "<tool_call>", "a stored message carries the call format")
		assert.NotContains(t, message.Content, "gitea_create_branch>")
	}
}

// A comment that names the agent is answered where it was written: the run is told what it was
// mentioned in, and the thread it came from hears how the run ended.
func TestHarnessMentionAnswersInTheThread(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	defer func(previous string) { setting.Agent.Username = previous }(setting.Agent.Username)
	setting.Agent.Username = agentUser.Name
	InitMentions()
	defer func() {
		notify_service.CommentListener = nil
		notify_service.ReviewListener = nil
		notify_service.IssueListener = nil
		notify_service.CodeCommentListener = nil
	}()

	model := newScriptedModel(t, "gitea_read_issue", `{"index":1}`, "the issue asks for a change")
	conversation, _ := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 1})
	mentioner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
	body := "@" + agentUser.Name + " what does this issue ask for"
	notify_service.CreateIssueComment(ctx, mentioner, repo, issue,
		&issues_model.Comment{ID: 1, IssueID: issue.ID, Content: body}, []*user_model.User{agentUser})

	// the mention starts its run in the session of that repository, and the run finishes here:
	// the report that settles the thread follows it
	var run *agent_model.Run
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		latest, err := LatestRun(ctx, conversation.ID)
		require.NoError(t, err)
		if latest != nil && !agent_model.IsRunActive(latest.Status) {
			run = latest
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, run, "the mention never started a run")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	told := false
	for _, request := range model.requestsMade() {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, body) {
				told = true
			}
		}
	}
	assert.True(t, told, "the run was never told about the mention")

	// the comment that said the work was under way carries the answer now
	require.NotZero(t, run.StatusCommentID)
	var note *issues_model.Comment
	for time.Now().Before(deadline) {
		settled, err := issues_model.GetCommentByID(ctx, run.StatusCommentID)
		if err == nil {
			note = settled
			if strings.Contains(note.Content, "the issue asks for a change") {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, note)
	assert.Equal(t, agentUser.ID, note.PosterID)
	assert.Contains(t, note.Content, "the issue asks for a change")
}

// A call that asks its reader first is answered by them, and what it was allowed to do really
// happened: the file the model wrote reads back from the branch it was committed to.
func TestHarnessApprovalLetsTheChangeLand(t *testing.T) {
	ctx := context.Background()
	model := newScriptedModel(t, "gitea_commit_files",
		`{"branch":"harness-approval","message":"add the changelog",`+
			`"files":[{"path":"CHANGELOG.md","content":"the approval landed\n"}]}`,
		"the change is committed")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_commit_files", agent_tools.PermissionManual, true))
	// the fixture hooks exec the built binary with a config a unit test never has, so the push
	// this spec really makes would be refused before the commit is written
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, os.RemoveAll(filepath.Join(gitrepo.RepoLocalPath(repo), "hooks")))

	_, run, err := SubmitUserMessage(ctx, conversation, user.ID, "add a changelog")
	require.NoError(t, err)
	require.NotNil(t, run)

	var approval *agent_model.ToolApproval
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pending, err := PendingApprovals(ctx, conversation.ID)
		require.NoError(t, err)
		if len(pending) > 0 {
			approval = pending[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, approval, "the write was never put to its reader")
	assert.Equal(t, "gitea_commit_files", approval.Tool)
	latest, err := LatestRun(ctx, conversation.ID)
	require.NoError(t, err)
	require.Equal(t, agent_model.RunStatusWaiting, latest.Status, "the run was put down, not held")

	require.NoError(t, ResolveApproval(ctx, conversation.ID, approval.ID, user.ID, true))
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		latest, err = LatestRun(ctx, conversation.ID)
		require.NoError(t, err)
		if !agent_model.IsRunActive(latest.Status) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Equal(t, agent_model.RunStatusSucceeded, latest.Status, latest.Error)

	settled := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).ID(approval.ID).Get(settled)
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, agent_model.ToolApprovalAllowed, settled.Status)

	gitRepo, err := git.OpenRepository(ctx, repo)
	require.NoError(t, err)
	defer gitRepo.Close()
	commit, err := gitRepo.GetBranchCommit(ctx, "harness-approval")
	require.NoError(t, err)
	entry, err := commit.GetTreeEntryByPath(ctx, gitRepo, "CHANGELOG.md")
	require.NoError(t, err)
	content, err := entry.Blob(gitRepo).GetBlobBytes(ctx, 8*1024)
	require.NoError(t, err)
	assert.Equal(t, "the approval landed\n", string(content))
}

// What the run is told about a change follows the tools it was offered: a session that cannot
// land one is told so, and one that can is told to decide what becomes of it.
func TestHarnessPromptCarriesTheLandingRule(t *testing.T) {
	ctx := context.Background()
	readsOnly := newScriptedModel(t, "gitea_read_file", `{"path":"README.md"}`, "the readme is short")
	conversation, user := startScriptedAgent(t, readsOnly, agent_model.ConversationKindRepository, 1)

	run := runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	requested := readsOnly.requestsMade()
	require.NotEmpty(t, requested)
	system := requested[0].Messages[0]
	require.Equal(t, "system", system.Role)
	assert.Contains(t, system.Content, "Nothing here changes anything")
	assert.NotContains(t, system.Content, "decide what becomes of it")

	lands := newScriptedModel(t, "gitea_read_file", `{"path":"README.md"}`, "the readme is short")
	conversation, user = startScriptedAgent(t, lands, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_create_pull_request", agent_tools.PermissionManual, true))
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_merge_pull_request", agent_tools.PermissionManual, true))

	run = runTurn(t, conversation, user, "read the readme")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	requested = lands.requestsMade()
	require.NotEmpty(t, requested)
	system = requested[0].Messages[0]
	require.Equal(t, "system", system.Role)
	assert.Contains(t, system.Content, "decide what becomes of it")
}

// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
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
	llm "gitea.dev/services/agent/llm"
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

// A run that can not go on without an answer asks for one: the question waits in the session, and
// what the reader writes back is what the call returned.
func TestHarnessAsksAQuestionAndUsesTheAnswer(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	model := newScriptedModel(t, agent_tools.AskUserToolName, `{"question":"Which branch should this land in?","options":"main|develop"}`, "landing it in develop")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	_, run, err := SubmitUserMessage(ctx, conversation, user.ID, "land the change somewhere")
	require.NoError(t, err)

	// the question waits for its reader instead of being answered by nobody
	var question *agent_model.ToolApproval
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && question == nil {
		pending, err := PendingApprovals(ctx, conversation.ID)
		require.NoError(t, err)
		if len(pending) > 0 {
			question = pending[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, question, "the question was never put to its reader")
	assert.Equal(t, agent_model.ToolApprovalKindQuestion, question.Kind, "a question is asked as one")
	assert.Contains(t, question.Arguments, "Which branch")

	require.NoError(t, AnswerQuestion(ctx, conversation.ID, question.ID, user, "develop"))
	finished := waitForRunToEnd(t, conversation.ID, run.ID)
	assert.Equal(t, agent_model.RunStatusSucceeded, finished.Status, finished.Error)

	// the answer reached the model as what the call returned, and is what it answered from
	asked := model.requestsMade()
	require.GreaterOrEqual(t, len(asked), 2)
	last := asked[len(asked)-1].Messages
	replayed := last[len(last)-1]
	assert.Equal(t, "tool", replayed.Role)
	assert.Contains(t, replayed.Content, "develop")
	final := messagesOf(t, conversation.ID)
	assert.Equal(t, "landing it in develop", final[len(final)-1].Content)

	// the session itself reads as a conversation: what was asked, and what was answered
	askedLine := ""
	answerLine := ""
	for _, message := range final {
		if strings.Contains(message.Content, "Asked the reader") {
			askedLine = message.Content
		}
		if strings.Contains(message.Content, "The reader answered: develop") {
			answerLine = message.Content
		}
	}
	assert.Contains(t, askedLine, "Which branch should this land in?")
	assert.Contains(t, askedLine, "main; develop", "the answers offered are part of what was asked")
	assert.NotEmpty(t, answerLine, "the session does not show what the reader answered")
}

// With nobody to ask, the decision model answers the question the run would have put to a person:
// autopilot works on, and the session says what was decided in its place.
func TestHarnessDecisionModelAnswersForAutopilot(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	model := newScriptedModel(t, agent_tools.AskUserToolName,
		`{"question":"Which branch should this land in?","options":"main|develop"}`, "landing it in develop")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	// the decision model, offered only to that role
	decider := newScriptedModel(t, "", "", "develop, because the change is not ready for main")
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	key, err := CreateAPIKey(ctx, owner.ID, "decision-key", "sk-decision")
	require.NoError(t, err)
	provider := &agent_model.Provider{UserID: owner.ID, Name: "decider", Provider: "openai", BaseURL: decider.url, Model: "decider", APIKeyID: key.ID}
	require.NoError(t, db.Insert(ctx, provider))
	require.NoError(t, SetModelRoleRef(ctx, agent_model.ModelRoleDecision, fmt.Sprintf("provider:%d/decider", provider.ID)))
	require.NoError(t, SetConversationAutopilot(ctx, conversation, true))

	run := runTurn(t, conversation, user, "land the change somewhere")
	assert.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	assert.NotEmpty(t, decider.requestsMade(), "the decision model was not asked to answer the question")

	// no reader was asked, and the session says what was decided instead
	pending, err := PendingApprovals(ctx, conversation.ID)
	require.NoError(t, err)
	assert.Empty(t, pending, "autopilot left a question for a reader")
	record := toolRecord(t, conversation.ID, agent_tools.AskUserToolName)
	assert.Contains(t, record, "Autopilot answered this instead of asking", "the session does not say the run decided")
	assert.Contains(t, record, "develop, because the change is not ready for main")

	// the answer is what the call returned, so the run goes on from it
	final := messagesOf(t, conversation.ID)
	require.NotEmpty(t, final)
	assert.Contains(t, final[len(final)-1].Content, "landing it in develop")
}

// The review a call gets before it runs on its own is a decision, so it answers from the
// decision role when an instance has set one: a decision takes a model worth asking.
func TestHarnessDecidesWithItsOwnRole(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	model := newScriptedModel(t, "gitea_create_branch", `{"branch":"review-role-branch","from_branch":"main","repository":"user2/repo1"}`, "the branch is there")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)

	// a second model, offered only to the review role
	review := newScriptedModel(t, "", "", "")
	review.review = `{"allow": true, "reason": "the request asks for a branch"}`
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	key, err := CreateAPIKey(ctx, owner.ID, "review-key", "sk-review")
	require.NoError(t, err)
	provider := &agent_model.Provider{UserID: owner.ID, Name: "review", Provider: "openai", BaseURL: review.url, Model: "review", APIKeyID: key.ID}
	require.NoError(t, db.Insert(ctx, provider))
	require.NoError(t, SetModelRoleRef(ctx, agent_model.ModelRoleDecision, fmt.Sprintf("provider:%d/review", provider.ID)))

	require.NoError(t, SetConversationAutopilot(ctx, conversation, true))
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_create_branch", agent_tools.PermissionManual, true))
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, os.RemoveAll(filepath.Join(gitrepo.RepoLocalPath(repo), "hooks")))

	run := runTurn(t, conversation, user, "put this on a branch of its own")
	assert.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	assert.NotEmpty(t, review.requestsMade(), "the review role was not asked to review the call")
	assert.Contains(t, toolRecord(t, conversation.ID, "gitea_create_branch"), "the request asks for a branch")
}

// Autopilot decides for a run instead of asking a reader: the call is reviewed by the model, it
// runs when the review says so, and what was decided stays in the transcript.
func TestHarnessAutopilotAllowsAManualCallWithoutAReader(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	// an instance reviews a call before autopilot would, so this spec turns the instance review off
	// to hold autopilot to the path of its own
	originalReview := setting.Agent.ReviewCalls
	setting.Agent.ReviewCalls = false
	t.Cleanup(func() { setting.Agent.ReviewCalls = originalReview })
	model := newScriptedModel(t, "gitea_create_branch", `{"branch":"autopilot-branch","from_branch":"main","repository":"user2/repo1"}`, "the branch is there")
	model.review = `{"allow": true, "reason": "the request asks for a branch of its own"}`
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, SetConversationAutopilot(ctx, conversation, true))
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_create_branch", agent_tools.PermissionManual, true))
	// the fixture hooks exec the built binary with a config a unit test never has
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, os.RemoveAll(filepath.Join(gitrepo.RepoLocalPath(repo), "hooks")))

	run := runTurn(t, conversation, user, "put this on a branch of its own")
	assert.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	pending, err := PendingApprovals(ctx, conversation.ID)
	require.NoError(t, err)
	assert.Empty(t, pending, "autopilot left a call waiting for a reader")

	record := toolRecord(t, conversation.ID, "gitea_create_branch")
	require.NotEmpty(t, record, "the call autopilot allowed did not run")
	assert.Contains(t, record, "autopilot allowed this call", "the transcript does not say who decided")
	assert.Contains(t, record, "the request asks for a branch of its own")
}

// A review that does not vouch for the call is a refusal: nothing runs on a decision nobody made.
func TestHarnessAutopilotRefusesWhatItsReviewRefuses(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	// an instance reviews a call before autopilot would, so this spec turns the instance review off
	// to hold autopilot to the path of its own
	originalReview := setting.Agent.ReviewCalls
	setting.Agent.ReviewCalls = false
	t.Cleanup(func() { setting.Agent.ReviewCalls = originalReview })
	model := newScriptedModel(t, "gitea_create_branch", `{"branch":"autopilot-branch","from_branch":"main","repository":"user2/repo1"}`, "I did not make the branch")
	model.review = `{"allow": false, "reason": "the request does not need a branch"}`
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, SetConversationAutopilot(ctx, conversation, true))
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_create_branch", agent_tools.PermissionManual, true))
	// the fixture hooks exec the built binary with a config a unit test never has
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, os.RemoveAll(filepath.Join(gitrepo.RepoLocalPath(repo), "hooks")))

	run := runTurn(t, conversation, user, "put this on a branch of its own")
	assert.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	refused := toolRecord(t, conversation.ID, "gitea_create_branch")
	assert.Contains(t, refused, "Autopilot refused this call", "the refusal is not in the transcript")
	assert.NotContains(t, refused, "created from", "a call autopilot refused ran anyway")
	final := messagesOf(t, conversation.ID)
	require.NotEmpty(t, final)
	answered := model.requestsMade()
	last := answered[len(answered)-1].Messages
	assert.Contains(t, last[len(last)-1].Content, "Autopilot refused this call: the request does not need a branch")
}

// A member that opens a stream and sends nothing is a member that is not answering: the run has
// to fail saying so, so the router falls over and the reader is not left with a session that
// waits. Read as a cancellation, it looked like the reader had stopped it.
func TestHarnessFailsARunWhoseMemberStaysSilent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	restore := llm.SetStreamLimitsForTest(200*time.Millisecond, 400*time.Millisecond, 20*time.Millisecond)
	defer restore()

	model := newScriptedModel(t, "", "", "never reached")
	model.silent = true
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindServer, 0)

	run := runTurn(t, conversation, user, "hello there")
	assert.Equal(t, agent_model.RunStatusFailed, run.Status, "a silent member ended the run as %q", run.Status)
	assert.Contains(t, run.Error, "sent nothing", "the run does not say why it failed")
	assert.Contains(t, run.Error, "scripted", "the run does not name the member it could not read")
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

	require.NoError(t, ResolveApproval(ctx, conversation.ID, approval.ID, user, true))
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

// A call that would wait for a reader is put to the decision model, and the run goes on by itself:
// what decided it is kept in the transcript, and where a reader's answer is kept, so the calls of a
// run are read back in one place whatever decided them.
func TestHarnessReviewsACallByDefault(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	assert.True(t, setting.Agent.ReviewCalls, "an instance reviews a call unless it is told not to")

	model := newScriptedModel(t, "gitea_create_branch", `{"branch":"reviewed-branch","from_branch":"main","repository":"user2/repo1"}`, "the branch is there")
	model.review = `{"allow": true, "reason": "the request asks for a branch of its own"}`
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_create_branch", agent_tools.PermissionManual, true))
	// the fixture hooks exec the built binary with a config a unit test never has
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, os.RemoveAll(filepath.Join(gitrepo.RepoLocalPath(repo), "hooks")))

	run := runTurn(t, conversation, user, "put this on a branch of its own")
	assert.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	pending, err := PendingApprovals(ctx, conversation.ID)
	require.NoError(t, err)
	assert.Empty(t, pending, "a reviewed call was left waiting for a reader")

	record := toolRecord(t, conversation.ID, "gitea_create_branch")
	require.NotEmpty(t, record, "the call the run reviewed did not run")
	assert.Contains(t, record, "the run reviewed this call and allowed it", "the transcript does not say who decided")
	assert.Contains(t, record, "the request asks for a branch of its own")

	decided := make([]*agent_model.ToolApproval, 0, 1)
	require.NoError(t, db.GetEngine(ctx).Where("conversation_id = ?", conversation.ID).Find(&decided))
	require.Len(t, decided, 1, "the decision was not kept where a reader's answer is kept")
	assert.Equal(t, agent_model.ToolApprovalAllowed, decided[0].Status)
	assert.EqualValues(t, 0, decided[0].DecidedUserID, "a reviewed call was recorded as a reader's decision")
	assert.Contains(t, decided[0].Result, "created from", "what the call returned was not kept with the decision")
}

// A review that does not vouch for a call stops it, and a call is kept as refused rather than run
// on a decision nobody made.
func TestHarnessRefusesWhatTheReviewRefuses(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	model := newScriptedModel(t, "gitea_create_branch", `{"branch":"reviewed-branch","from_branch":"main","repository":"user2/repo1"}`, "I did not make the branch")
	model.review = `{"allow": false, "reason": "the request does not need a branch"}`
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_create_branch", agent_tools.PermissionManual, true))
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, os.RemoveAll(filepath.Join(gitrepo.RepoLocalPath(repo), "hooks")))

	run := runTurn(t, conversation, user, "put this on a branch of its own")
	assert.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)

	refused := toolRecord(t, conversation.ID, "gitea_create_branch")
	assert.Contains(t, refused, "the run reviewed this call and refused it")
	assert.Contains(t, refused, "the request does not need a branch")
	assert.NotContains(t, refused, "created from", "a call a review refused ran anyway")

	decided := make([]*agent_model.ToolApproval, 0, 1)
	require.NoError(t, db.GetEngine(ctx).Where("conversation_id = ?", conversation.ID).Find(&decided))
	require.Len(t, decided, 1)
	assert.Equal(t, agent_model.ToolApprovalRefused, decided[0].Status)
}

// A review that cannot be read is not a decision: the call is put to a reader after all, so nothing
// runs on a review that said nothing.
func TestHarnessWaitsForAReaderWhenTheReviewSaysNothing(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()

	// the model answers the review as it answers anything else: with a call, which is no decision
	model := newScriptedModel(t, "gitea_create_branch", `{"branch":"reviewed-branch","from_branch":"main","repository":"user2/repo1"}`, "the branch is there")
	conversation, user := startScriptedAgent(t, model, agent_model.ConversationKindRepository, 1)
	require.NoError(t, agent_tools.SetMode(ctx, "gitea_create_branch", agent_tools.PermissionManual, true))
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	require.NoError(t, os.RemoveAll(filepath.Join(gitrepo.RepoLocalPath(repo), "hooks")))

	_, run, err := SubmitUserMessage(ctx, conversation, user.ID, "put this on a branch of its own")
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
	require.NotNil(t, approval, "a review that said nothing did not leave the call to a reader")
	assert.Equal(t, "gitea_create_branch", approval.Tool)
	assert.Empty(t, toolRecord(t, conversation.ID, "gitea_create_branch"), "a call ran on a review that said nothing")
}

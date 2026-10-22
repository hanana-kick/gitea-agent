// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/log"
	"gitea.dev/modules/timeutil"
	llm "gitea.dev/services/agent/llm"
	agent_skills "gitea.dev/services/agent/skills"
	agent_tools "gitea.dev/services/agent/tools"
)

const (
	engineWorkers      = 4
	engineQueueSize    = 256
	engineRunTimeout   = 10 * time.Minute
	engineHistoryLimit = 40
	enginePromptLimit  = 100 * 1024 // cap the transcript handed to the model

	// engineToolRounds caps how often one answer may stop to call tools before it has
	// to answer without them, so a model that keeps calling still finishes.
	engineToolRounds = 6
	// engineApprovalWait is how long a manual tool call waits for its user.
	engineApprovalWait = 5 * time.Minute

	engineNameHistoryLimit = 8
	engineTitleTimeout     = 60 * time.Second
	engineTitleExcerpt     = 500
	// the skill list in the prompt is capped by count and by size, so skills cost the
	// prompt a few lines however many the instance stores
	skillCatalogLimit = 24 // skills, not lines
	skillCatalogBytes = 2 * 1024
	skillLineLimit    = 140

	// the resident summarises recent work, so it reads further back and spends its
	// excerpt budget from the newest exchange backwards
	engineSummaryHistoryLimit = 20
	engineSummaryBudget       = 1600
	engineSummaryNewest       = 700
	engineSummaryOlder        = 160
)

// Event names published for one conversation.
const (
	EventMessage      = "message"      // a transcript entry was appended
	EventRun          = "run"          // a run changed state
	EventConversation = "conversation" // the session was renamed or given an icon
	EventDelta        = "delta"        // the answer is arriving piece by piece
	EventApproval     = "approval"     // a tool call waits for the reader to allow it
)

// ConversationState is the part of a session that clients show in a list.
type ConversationState struct {
	ID int64 `json:"id"`
	// Summary is what the session worked on lately; the resident shows it instead of its name.
	Summary string `json:"summary"`
	Title   string `json:"title"`
	Icon    string `json:"icon"`
}

// State renders a session for the clients watching it.
func State(conversation *agent_model.Conversation) ConversationState {
	return ConversationState{ID: conversation.ID, Title: conversation.Title, Summary: conversation.Summary, Icon: conversation.Icon}
}

// Event is one update for the clients watching a conversation.
type Event struct {
	Name string
	Data any
}

// DeltaState is one piece of the answer a run is streaming.
type DeltaState struct {
	RunID     int64  `json:"runId"`
	Content   string `json:"content,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	Reset     bool   `json:"reset,omitempty"` // drop what was shown: another member starts over
}

// RunState is the part of a run that clients need.
type RunState struct {
	ID             int64  `json:"id"`
	ConversationID int64  `json:"conversationId"`
	Status         string `json:"status"`
	Error          string `json:"error"`
	Cancelable     bool   `json:"cancelable"`
}

// RunStateOf renders a run for the clients watching it.
func RunStateOf(run *agent_model.Run) RunState {
	return RunState{
		ID:             run.ID,
		ConversationID: run.ConversationID,
		Status:         run.Status,
		Error:          run.Error,
		Cancelable:     agent_model.IsRunActive(run.Status),
	}
}

// ErrNoActiveRun is returned when there is nothing to cancel.
type ErrNoActiveRun struct{}

func (ErrNoActiveRun) Error() string { return "no run is active" }

// Is reports whether target is an ErrNoActiveRun.
func (ErrNoActiveRun) Is(target error) bool {
	_, ok := target.(ErrNoActiveRun)
	return ok
}

// Engine runs agent work outside any request, so a client may disconnect right after
// sending and the answer is still produced and stored. Work uses a background context
// because it outlives the request that started it.
type Engine struct {
	queue  chan int64
	broker *eventBroker

	mu        sync.Mutex
	cancels   map[int64]context.CancelFunc // conversation ID -> cancel of the active run
	locks     map[int64]*sync.Mutex        // serializes the runs of one conversation
	approvals map[int64]chan bool          // approval ID -> the waiting run
}

var engine = &Engine{
	queue:     make(chan int64, engineQueueSize),
	broker:    newEventBroker(),
	cancels:   make(map[int64]context.CancelFunc),
	locks:     make(map[int64]*sync.Mutex),
	approvals: make(map[int64]chan bool),
}

// InitEngine recovers the runs a restart interrupted and starts the workers.
func InitEngine(ctx context.Context) error {
	if err := recoverRuns(ctx); err != nil {
		return err
	}
	for range engineWorkers {
		go engine.work()
	}
	return nil
}

// recoverRuns fails the runs that were in flight and re-queues the ones that never started.
func recoverRuns(ctx context.Context) error {
	if _, err := db.GetEngine(ctx).
		Where("status = ?", agent_model.RunStatusRunning).
		Cols("status", "error", "finished_unix").
		Update(&agent_model.Run{
			Status:       agent_model.RunStatusFailed,
			Error:        "interrupted by a server restart",
			FinishedUnix: timeutil.TimeStampNow(),
		}); err != nil {
		return err
	}

	runs := make([]*agent_model.Run, 0, 8)
	if err := db.GetEngine(ctx).Where("status = ?", agent_model.RunStatusQueued).OrderBy("id ASC").Find(&runs); err != nil {
		return err
	}
	for _, run := range runs {
		engine.enqueue(run.ID)
	}
	if len(runs) > 0 {
		log.Info("Agent engine: re-queued %d run(s) after restart", len(runs))
	}
	return nil
}

// SubmitUserMessage stores a user message and starts a run for it. The caller does not
// wait for the answer. The stored message is returned so files can be attached to it.
func SubmitUserMessage(ctx context.Context, conversation *agent_model.Conversation, userID int64, content string) (*agent_model.Message, *agent_model.Run, error) {
	message, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleUser, content)
	if err != nil {
		return nil, nil, err
	}
	engine.broker.publish(conversation.ID, Event{Name: EventMessage, Data: message.ID})

	run, err := StartRun(ctx, conversation, userID)
	if err != nil {
		return nil, nil, err
	}
	return message, run, nil
}

// StartRun queues a new run for a conversation. It is used when a message only needs an
// answer, e.g. after an edit rewound the transcript. userID is who asked for it: the
// tools of the run read only what that user may see.
func StartRun(ctx context.Context, conversation *agent_model.Conversation, userID int64) (*agent_model.Run, error) {
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: userID, Status: agent_model.RunStatusQueued}
	if err := db.Insert(ctx, run); err != nil {
		return nil, err
	}
	engine.enqueue(run.ID)
	engine.publishRun(conversation.ID, run)
	return run, nil
}

// CancelRun stops the run of a conversation, if one is active.
func CancelRun(conversationID int64) error {
	engine.mu.Lock()
	cancel := engine.cancels[conversationID]
	engine.mu.Unlock()
	if cancel != nil {
		cancel()
		return nil
	}

	// a queued run has no cancel func yet: finish it here, its worker then finds nothing to do
	ctx := context.Background()
	run, err := ActiveRun(ctx, conversationID)
	if err != nil {
		return err
	}
	if run == nil {
		return ErrNoActiveRun{}
	}
	if run.Status != agent_model.RunStatusQueued {
		return ErrNoActiveRun{}
	}
	run.Status = agent_model.RunStatusCanceled
	run.FinishedUnix = timeutil.TimeStampNow()
	if _, err := db.GetEngine(ctx).ID(run.ID).Cols("status", "finished_unix").Update(run); err != nil {
		return err
	}
	engine.publishRun(run.ConversationID, run)
	return nil
}

// ActiveRun returns the run of a conversation that is still going, if any.
func ActiveRun(ctx context.Context, conversationID int64) (*agent_model.Run, error) {
	run := &agent_model.Run{}
	exist, err := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		In("status", []string{agent_model.RunStatusQueued, agent_model.RunStatusRunning}).
		OrderBy("id DESC").
		Get(run)
	if err != nil || !exist {
		return nil, err
	}
	return run, nil
}

// BusyConversationIDs returns the conversations that wait for or write an answer.
func BusyConversationIDs(ctx context.Context) (map[int64]bool, error) {
	ids := make([]int64, 0, 8)
	if err := db.GetEngine(ctx).Table("agent_run").
		In("status", []string{agent_model.RunStatusQueued, agent_model.RunStatusRunning}).
		Cols("conversation_id").Find(&ids); err != nil {
		return nil, fmt.Errorf("find busy conversations: %w", err)
	}
	busy := make(map[int64]bool, len(ids))
	for _, id := range ids {
		busy[id] = true
	}
	return busy, nil
}

// LatestRun returns the most recent run of a conversation, whatever its state.
func LatestRun(ctx context.Context, conversationID int64) (*agent_model.Run, error) {
	run := &agent_model.Run{}
	exist, err := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		OrderBy("id DESC").
		Get(run)
	if err != nil || !exist {
		return nil, err
	}
	return run, nil
}

// NotifyConversation publishes the current title and icon of a session.
func NotifyConversation(conversation *agent_model.Conversation) {
	engine.broker.publish(conversation.ID, Event{Name: EventConversation, Data: State(conversation)})
}

// Subscribe attaches to the updates of a conversation. The returned function detaches.
func Subscribe(conversationID int64) (<-chan Event, func()) {
	return engine.broker.subscribe(conversationID)
}

// SubscribeRunEvents attaches to the run changes of every conversation.
func SubscribeRunEvents() (<-chan Event, func()) {
	return engine.broker.subscribeAll()
}

func (e *Engine) enqueue(runID int64) {
	select {
	case e.queue <- runID:
	default:
		// the queue is full: fail the run loudly instead of losing it
		ctx := context.Background()
		if _, err := db.GetEngine(ctx).ID(runID).Cols("status", "error", "finished_unix").
			Update(&agent_model.Run{Status: agent_model.RunStatusFailed, Error: "the agent queue is full", FinishedUnix: timeutil.TimeStampNow()}); err != nil {
			log.Error("Agent engine: unable to fail run %d: %v", runID, err)
		}
	}
}

func (e *Engine) work() {
	for runID := range e.queue {
		e.execute(runID)
	}
}

// execute runs one stored run to completion, whatever the state of its conversation's clients.
func (e *Engine) execute(runID int64) {
	ctx := context.Background()
	run, exist, err := db.GetByID[agent_model.Run](ctx, runID)
	if err != nil || !exist {
		log.Error("Agent engine: unable to load run %d: %v", runID, err)
		return
	}
	if run.Status != agent_model.RunStatusQueued {
		return // cancelled while it waited in the queue
	}
	conversation, exist, err := db.GetByID[agent_model.Conversation](ctx, run.ConversationID)
	if err != nil || !exist {
		e.finishRun(ctx, run, agent_model.RunStatusFailed, "the session no longer exists")
		return
	}

	lock := e.conversationLock(conversation.ID)
	lock.Lock()
	defer lock.Unlock()

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), engineRunTimeout)
	defer cancel()
	e.setCancel(conversation.ID, cancel)
	defer e.clearCancel(conversation.ID)

	if _, err := db.GetEngine(ctx).ID(run.ID).Cols("status", "started_unix").
		Update(&agent_model.Run{Status: agent_model.RunStatusRunning, StartedUnix: timeutil.TimeStampNow()}); err != nil {
		log.Error("Agent engine: unable to start run %d: %v", run.ID, err)
		return
	}
	run.Status = agent_model.RunStatusRunning
	e.publishRun(conversation.ID, run)

	reply, model, err := e.answer(runCtx, conversation, run)
	if err != nil {
		status := agent_model.RunStatusFailed
		reason := err.Error()
		// every member the router tried is part of the reason, so the swap is visible
		if route, ok := AsRoute(err); ok {
			if failures := route.Failures(); len(failures) > 0 {
				reason = strings.Join(failures, "; ")
			}
		}
		if errors.Is(runCtx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			status = agent_model.RunStatusCanceled
			reason = ""
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			reason = "the model did not answer in time"
		}
		e.finishRun(ctx, run, status, reason)
		return
	}

	if strings.TrimSpace(reply) == "" {
		e.finishRun(ctx, run, agent_model.RunStatusFailed, "the model returned an empty answer")
		return
	}
	message, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleAssistant, reply)
	if err != nil {
		e.finishRun(ctx, run, agent_model.RunStatusFailed, err.Error())
		return
	}
	e.broker.publish(conversation.ID, Event{Name: EventMessage, Data: message.ID})

	run.Model = model
	e.finishRun(ctx, run, agent_model.RunStatusSucceeded, "")
	e.nameConversation(ctx, conversation)
}

func messageIDs(messages []*agent_model.Message) []int64 {
	ids := make([]int64, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}

// nameConversation names a new session, or refreshes what the resident has been working on.
func (e *Engine) nameConversation(ctx context.Context, conversation *agent_model.Conversation) {
	resident := conversation.Kind == agent_model.ConversationKindResident
	if !resident && conversation.TitleSet {
		return // the resident keeps its name, so it is summarized on every exchange
	}
	ref, err := ModelRoleRef(ctx, agent_model.ModelRoleSummary)
	if err != nil || ref == "" {
		return // naming is a convenience, the session keeps its placeholder without it
	}
	historyLimit := engineNameHistoryLimit
	if resident {
		historyLimit = engineSummaryHistoryLimit
	}
	history, err := ListMessages(ctx, conversation.ID, historyLimit)
	if err != nil || len(history) < 2 {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), engineTitleTimeout)
	defer cancel()

	system, ask := "You name chat conversations. Answer with a title of at most six "+
		"words, no quotes, no punctuation at the end, in the language the user writes in.",
		"Name this conversation:\n\n"
	if resident {
		system, ask = "You summarize the latest work of an agent. Focus on the most recent "+
			"exchange of the transcript; everything before it is only context. Answer with at most six "+
			"words, no quotes, no punctuation at the end, in the language the user writes in.",
			"What has the agent been working on most recently:\n\n"
	}
	prompt := []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: ask + conversationExcerpt(history, resident)},
	}
	title, _, err := Answer(ctx, ref, llm.ChatRequest{Messages: prompt})
	if err != nil {
		log.Warn("Agent engine: unable to name session %d: %v", conversation.ID, err)
		return
	}
	title = cleanConversationTitle(title)
	if title == "" {
		log.Debug("Agent engine: session %d keeps its placeholder title, the model named nothing", conversation.ID)
		return
	}
	store := SetConversationTitle
	if resident {
		store = SetConversationSummary
	}
	if err := store(ctx, conversation, title); err != nil {
		log.Warn("Agent engine: unable to store name of session %d: %v", conversation.ID, err)
		return
	}
	e.broker.publish(conversation.ID, Event{Name: EventConversation, Data: State(conversation)})
}

// conversationExcerpt renders a transcript excerpt for the summary request. Conversation
// naming reads the opening; the resident summarises recent work, so its budget is spent
// from the newest exchange backwards and earlier messages only give context.
func conversationExcerpt(history []*agent_model.Message, resident bool) string {
	messages := make([]*agent_model.Message, 0, len(history))
	for _, message := range history {
		// tool records and internal notes are for the reader; the answer already reflects
		// what a tool returned, so they would only crowd the excerpt
		if message.Role == agent_model.MessageRoleSystem || message.Role == agent_model.MessageRoleTool {
			continue
		}
		if content := strings.TrimSpace(message.Content); content != "" {
			messages = append(messages, message)
		}
	}
	if len(messages) == 0 {
		return ""
	}

	if !resident {
		var builder strings.Builder
		for _, message := range messages {
			line, used := excerptLine(message, engineTitleExcerpt, engineSummaryBudget)
			if used == 0 {
				break
			}
			builder.WriteString(line)
		}
		return builder.String()
	}

	lastUser := -1
	for i, message := range messages {
		if message.Role == agent_model.MessageRoleUser {
			lastUser = i
		}
	}

	var parts []string
	remaining := engineSummaryBudget
	for i := range slices.Backward(messages) {
		limit := engineSummaryOlder
		if i >= lastUser {
			limit = engineSummaryNewest // the newest exchange carries the meaning
		}
		line, used := excerptLine(messages[i], limit, remaining)
		if used == 0 {
			break
		}
		remaining -= used
		parts = append(parts, line)
	}
	slices.Reverse(parts)
	return strings.Join(parts, "\n")
}

// excerptLine renders one transcript line, capped per message and by the budget left.
func excerptLine(message *agent_model.Message, limit, remaining int) (string, int) {
	content := message.Content
	if runes := []rune(content); len(runes) > limit {
		content = string(runes[:limit]) + "…"
	}
	line := message.Role + ": " + strings.TrimSpace(content) + "\n"
	runes := []rune(line)
	if len(runes) > remaining {
		if remaining <= 0 {
			return "", 0
		}
		runes = runes[:remaining]
	}
	return string(runes), len(runes)
}

// cleanConversationTitle drops the decorations a model likes to add around a title.
func cleanConversationTitle(title string) string {
	title = strings.TrimSpace(title)
	title = strings.Trim(title, "\"'`*#” “")
	title = strings.SplitN(title, "\n", 2)[0]
	title = strings.TrimSpace(title)
	if runes := []rune(title); len(runes) > 80 {
		title = strings.TrimSpace(string(runes[:80]))
	}
	return title
}

// answer builds the prompt from the stored transcript and asks the session model, making
// the tool calls it asks for until it answers.
func (e *Engine) answer(ctx context.Context, conversation *agent_model.Conversation, run *agent_model.Run) (string, string, error) {
	role := ModelRoleForKind(conversation.Kind)
	ref, err := ModelRoleRef(ctx, role)
	if err != nil {
		return "", "", err
	}
	if ref == "" {
		return "", "", ErrModelRoleNotSet{Role: role}
	}
	history, err := ListMessages(ctx, conversation.ID, engineHistoryLimit)
	if err != nil {
		return "", "", err
	}
	attachments, err := ListMessageAttachments(ctx, messageIDs(history))
	if err != nil {
		return "", "", err
	}
	// a run with no known user has nobody's rights to read with, so it gets no tools
	doer, err := runUser(ctx, run)
	if err != nil && !errors.Is(err, errRunUserUnknown{}) {
		return "", "", err
	}
	offerings, err := agent_tools.Available(ctx, conversation.Kind)
	if err != nil {
		return "", "", err
	}
	if doer == nil {
		offerings = nil
	}
	// every piece goes out at once, so the browser can show the answer while it is written
	onDelta := func(delta llm.Delta) error {
		if err := ctx.Err(); err != nil {
			return err // stop reading when the run was cancelled
		}
		e.broker.publish(conversation.ID, Event{Name: EventDelta, Data: DeltaState{
			RunID:     run.ID,
			Content:   delta.Content,
			Reasoning: delta.Reasoning,
			Reset:     delta.Reset,
		}})
		return nil
	}

	// the skills of this session reach the prompt as a catalog; their steps arrive only
	// when the model opens one. Without the tool that opens them the list would be a
	// promise the session cannot keep, so it stays out too.
	catalog, err := agent_skills.List(ctx, conversation.Kind)
	if err != nil {
		return "", "", err
	}
	if !slices.ContainsFunc(offerings, func(tool agent_tools.Tool) bool { return tool.Name() == agent_tools.SkillToolName }) {
		catalog = nil
	}
	messages := buildPrompt(ctx, conversation, history, attachments, offerings, catalog)
	defs := agent_tools.Definitions(offerings)
	model := ""
	for round := range engineToolRounds {
		// the last round goes without tools, so the model has to answer with what it read
		askedFor := defs
		if round == engineToolRounds-1 {
			askedFor = nil
		}
		message, answered, err := AnswerMessage(ctx, ref, llm.ChatRequest{Messages: messages, Tools: askedFor}, onDelta)
		if err != nil {
			return "", model, err
		}
		model = answered
		if len(message.ToolCalls) == 0 {
			return message.Content, model, nil
		}
		// the calls are what the reader watches happen; the answer starts after them
		e.broker.publish(conversation.ID, Event{Name: EventDelta, Data: DeltaState{RunID: run.ID, Reset: true}})
		messages = append(messages, *message)
		for _, call := range message.ToolCalls {
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    e.callTool(ctx, conversation, run, doer, call),
				ToolCallID: call.ID,
			})
		}
	}
	return "", model, errors.New("the agent kept calling tools without answering")
}

// errRunUserUnknown is a run whose asking user is gone or was never recorded; such a run
// has nobody's rights to read with, so it gets no tools.
type errRunUserUnknown struct{}

func (errRunUserUnknown) Error() string { return "the user of this run is unknown" }

// runUser returns the user whose message started the run.
func runUser(ctx context.Context, run *agent_model.Run) (*user_model.User, error) {
	if run.UserID == 0 {
		return nil, errRunUserUnknown{}
	}
	user, err := user_model.GetUserByID(ctx, run.UserID)
	if err != nil {
		if user_model.IsErrUserNotExist(err) {
			return nil, errRunUserUnknown{}
		}
		return nil, err
	}
	return user, nil
}

// callTool makes one call the model asked for, records it in the transcript and returns
// what the model gets to read.
func (e *Engine) callTool(ctx context.Context, conversation *agent_model.Conversation, run *agent_model.Run, doer *user_model.User, call llm.ToolCall) string {
	tool, decision, err := agent_tools.Check(ctx, conversation.Kind, call.Function.Name)
	switch {
	case err != nil:
		return e.recordTool(ctx, conversation, call, err.Error())
	case decision == agent_tools.DecisionDenied:
		return e.recordTool(ctx, conversation, call, "That tool is switched off for this session.")
	case doer == nil:
		return e.recordTool(ctx, conversation, call, "No user belongs to this session, so tools cannot run.")
	}
	if decision == agent_tools.DecisionAsk {
		if allowed, reason := e.awaitApproval(ctx, conversation, run, call); !allowed {
			return e.recordTool(ctx, conversation, call, reason)
		}
	}
	output, err := tool.Run(ctx, doer, agent_tools.Session{Kind: conversation.Kind}, call.Function.Arguments)
	if err != nil {
		log.Error("Agent engine: tool %s of run %d failed: %v", call.Function.Name, run.ID, err)
		return e.recordTool(ctx, conversation, call, "The tool could not run: "+err.Error())
	}
	return e.recordTool(ctx, conversation, call, output)
}

// recordTool puts one call and its result in the transcript, and hands the same result
// back for the model to read.
func (e *Engine) recordTool(ctx context.Context, conversation *agent_model.Conversation, call llm.ToolCall, result string) string {
	args := strings.Join(strings.Fields(call.Function.Arguments), " ")
	if message, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleTool, argsLine(call.Function.Name, args)+"\n"+result); err == nil {
		e.broker.publish(conversation.ID, Event{Name: EventMessage, Data: message.ID})
	} else {
		log.Error("Agent engine: unable to record tool %s: %v", call.Function.Name, err)
	}
	return result
}

// argsLine renders a call the way the transcript shows it.
func argsLine(name, args string) string {
	if args == "" || args == "{}" {
		return name
	}
	return name + " " + args
}

// awaitApproval puts a manual call in front of the reader and waits for the answer. The
// worker waits with it, which is why the wait is bounded and cancellable.
func (e *Engine) awaitApproval(ctx context.Context, conversation *agent_model.Conversation, run *agent_model.Run, call llm.ToolCall) (bool, string) {
	approval := &agent_model.ToolApproval{
		ConversationID: conversation.ID,
		RunID:          run.ID,
		Tool:           call.Function.Name,
		Arguments:      call.Function.Arguments,
		Status:         agent_model.ToolApprovalPending,
	}
	if err := db.Insert(ctx, approval); err != nil {
		log.Error("Agent engine: unable to store tool approval: %v", err)
		return false, "The call could not be put to the user."
	}

	answer := make(chan bool, 1)
	e.mu.Lock()
	e.approvals[approval.ID] = answer
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.approvals, approval.ID)
		e.mu.Unlock()
	}()
	e.broker.publish(conversation.ID, Event{Name: EventApproval, Data: approval})

	select {
	case allowed := <-answer:
		if allowed {
			return true, ""
		}
		return false, "The user did not allow that call."
	case <-ctx.Done():
		e.expireApproval(approval.ID)
		return false, "The run ended before anyone answered."
	case <-time.After(engineApprovalWait):
		e.expireApproval(approval.ID)
		return false, "Nobody answered the request in time."
	}
}

// expireApproval closes a call nobody answered.
func (e *Engine) expireApproval(approvalID int64) {
	if _, err := db.GetEngine(context.Background()).ID(approvalID).
		Cols("status", "decided_unix").
		Update(&agent_model.ToolApproval{Status: agent_model.ToolApprovalExpired, DecidedUnix: timeutil.TimeStampNow()}); err != nil {
		log.Error("Agent engine: unable to expire tool approval %d: %v", approvalID, err)
	}
}

// ErrApprovalNotPending is returned when a call has already been answered.
type ErrApprovalNotPending struct{}

func (ErrApprovalNotPending) Error() string { return "that call has already been answered" }

// Is reports whether target is an ErrApprovalNotPending.
func (ErrApprovalNotPending) Is(target error) bool {
	_, ok := target.(ErrApprovalNotPending)
	return ok
}

// PendingApprovals returns the calls of a session still waiting for a reader.
func PendingApprovals(ctx context.Context, conversationID int64) ([]*agent_model.ToolApproval, error) {
	approvals := make([]*agent_model.ToolApproval, 0, 4)
	err := db.GetEngine(ctx).
		Where("conversation_id = ?", conversationID).
		And("status = ?", agent_model.ToolApprovalPending).
		OrderBy("id ASC").
		Find(&approvals)
	return approvals, err
}

// ResolveApproval records what the reader decided about a pending call and wakes the run
// waiting for it. A call left pending by a restart is still closed here.
func ResolveApproval(ctx context.Context, conversationID, approvalID, decidedBy int64, allowed bool) error {
	approval := &agent_model.ToolApproval{}
	exist, err := db.GetEngine(ctx).ID(approvalID).
		And("conversation_id = ?", conversationID).
		Get(approval)
	if err != nil {
		return err
	}
	if !exist || approval.Status != agent_model.ToolApprovalPending {
		return ErrApprovalNotPending{}
	}
	run, exist, err := db.GetByID[agent_model.Run](ctx, approval.RunID)
	if err != nil {
		return err
	}
	// only the person whose question started the run answers for it
	if exist && run.UserID != 0 && run.UserID != decidedBy {
		return ErrApprovalNotOwner{}
	}
	status := agent_model.ToolApprovalRefused
	if allowed {
		status = agent_model.ToolApprovalAllowed
	}
	if _, err := db.GetEngine(ctx).ID(approvalID).Cols("status", "decided_user_id", "decided_unix").
		Update(&agent_model.ToolApproval{Status: status, DecidedUserID: decidedBy, DecidedUnix: timeutil.TimeStampNow()}); err != nil {
		return err
	}

	engine.mu.Lock()
	waiting := engine.approvals[approvalID]
	engine.mu.Unlock()
	if waiting != nil {
		waiting <- allowed // buffered: the run may already have moved on
	}
	return nil
}

// ErrApprovalNotOwner is returned when someone other than the requester answers a call.
type ErrApprovalNotOwner struct{}

func (ErrApprovalNotOwner) Error() string { return "only the user who asked may answer that call" }

// Is reports whether target is an ErrApprovalNotOwner.
func (ErrApprovalNotOwner) Is(target error) bool {
	_, ok := target.(ErrApprovalNotOwner)
	return ok
}

func (e *Engine) finishRun(ctx context.Context, run *agent_model.Run, status, reason string) {
	run.Status = status
	run.Error = reason
	run.FinishedUnix = timeutil.TimeStampNow()
	if _, err := db.GetEngine(ctx).ID(run.ID).
		Cols("status", "error", "model", "finished_unix").
		Update(run); err != nil {
		log.Error("Agent engine: unable to finish run %d: %v", run.ID, err)
		return
	}
	e.publishRun(run.ConversationID, run)
}

func (e *Engine) publishRun(conversationID int64, run *agent_model.Run) {
	e.broker.publish(conversationID, Event{Name: EventRun, Data: RunStateOf(run)})
}

func (e *Engine) setCancel(conversationID int64, cancel context.CancelFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancels[conversationID] = cancel
}

func (e *Engine) clearCancel(conversationID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.cancels, conversationID)
}

func (e *Engine) conversationLock(conversationID int64) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	lock := e.locks[conversationID]
	if lock == nil {
		lock = &sync.Mutex{}
		e.locks[conversationID] = lock
	}
	return lock
}

// eventBroker fans conversation updates out to the clients currently watching them.
type eventBroker struct {
	mu     sync.Mutex
	subs   map[int64]map[chan Event]struct{}
	global map[chan Event]struct{} // every run change, for the session list
}

func newEventBroker() *eventBroker {
	return &eventBroker{
		subs:   make(map[int64]map[chan Event]struct{}),
		global: make(map[chan Event]struct{}),
	}
}

func (b *eventBroker) subscribe(conversationID int64) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	if b.subs[conversationID] == nil {
		b.subs[conversationID] = make(map[chan Event]struct{})
	}
	b.subs[conversationID][ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[conversationID][ch]; ok {
			delete(b.subs[conversationID], ch)
			close(ch)
		}
	}
}

// publish never blocks: a slow client misses the event and catches up from the transcript.
func (b *eventBroker) publish(conversationID int64, event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[conversationID] {
		select {
		case ch <- event:
		default:
		}
	}
	// run changes reach every page: the session list marks which conversation is answering
	if event.Name != EventRun {
		return
	}
	for ch := range b.global {
		select {
		case ch <- event:
		default:
		}
	}
}

// subscribeAll listens to the run changes of every conversation.
func (b *eventBroker) subscribeAll() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.global[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.global[ch]; ok {
			delete(b.global, ch)
			close(ch)
		}
	}
}

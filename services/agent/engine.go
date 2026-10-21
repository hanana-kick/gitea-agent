// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/log"
	"gitea.dev/modules/timeutil"
	llm "gitea.dev/services/agent/llm"
)

const (
	engineWorkers      = 4
	engineQueueSize    = 256
	engineRunTimeout   = 10 * time.Minute
	engineHistoryLimit = 40
	enginePromptLimit  = 100 * 1024 // cap the transcript handed to the model

	engineNameHistoryLimit = 8
	engineTitleTimeout     = 60 * time.Second
	engineTitleExcerpt     = 500
)

// Event names published for one conversation.
const (
	EventMessage      = "message"      // a transcript entry was appended
	EventRun          = "run"          // a run changed state
	EventConversation = "conversation" // the session was renamed or given an icon
	EventDelta        = "delta"        // the answer is arriving piece by piece
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
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	Cancelable bool   `json:"cancelable"`
}

// RunStateOf renders a run for the clients watching it.
func RunStateOf(run *agent_model.Run) RunState {
	return RunState{
		ID:         run.ID,
		Status:     run.Status,
		Error:      run.Error,
		Cancelable: agent_model.IsRunActive(run.Status),
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

	mu      sync.Mutex
	cancels map[int64]context.CancelFunc // conversation ID -> cancel of the active run
	locks   map[int64]*sync.Mutex        // serializes the runs of one conversation
}

var engine = &Engine{
	queue:   make(chan int64, engineQueueSize),
	broker:  newEventBroker(),
	cancels: make(map[int64]context.CancelFunc),
	locks:   make(map[int64]*sync.Mutex),
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
func SubmitUserMessage(ctx context.Context, conversation *agent_model.Conversation, content string) (*agent_model.Message, *agent_model.Run, error) {
	message, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleUser, content)
	if err != nil {
		return nil, nil, err
	}
	engine.broker.publish(conversation.ID, Event{Name: EventMessage, Data: message.ID})

	run, err := StartRun(ctx, conversation)
	if err != nil {
		return nil, nil, err
	}
	return message, run, nil
}

// StartRun queues a new run for a conversation. It is used when a message only needs an
// answer, e.g. after an edit rewound the transcript.
func StartRun(ctx context.Context, conversation *agent_model.Conversation) (*agent_model.Run, error) {
	run := &agent_model.Run{ConversationID: conversation.ID, Status: agent_model.RunStatusQueued}
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
	if cancel == nil {
		return ErrNoActiveRun{}
	}
	cancel()
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

// nameConversation asks the model for a title once the first answer exists, so a session
// stops being called "new conversation". Failures keep the placeholder title.
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
	history, err := ListMessages(ctx, conversation.ID, engineNameHistoryLimit)
	if err != nil || len(history) < 2 {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), engineTitleTimeout)
	defer cancel()

	system, ask := "You name chat conversations. Answer with a title of at most six "+
		"words, no quotes, no punctuation at the end, in the language the user writes in.",
		"Name this conversation:\n\n"
	if resident {
		system, ask = "You summarize the latest work of an agent. Answer with at most six words, "+
			"no quotes, no punctuation at the end, in the language the user writes in.",
			"What is the agent working on:\n\n"
	}
	prompt := []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: ask + conversationExcerpt(history)},
	}
	// no max token limit: models that think before answering would spend a small budget on
	// reasoning and return no content at all
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

// conversationExcerpt renders the start of a transcript for the naming request.
func conversationExcerpt(history []*agent_model.Message) string {
	var builder strings.Builder
	added := 0
	for _, message := range history {
		if message.Role == agent_model.MessageRoleSystem {
			continue
		}
		content := strings.TrimSpace(message.Content)
		if content == "" {
			continue
		}
		if runes := []rune(content); len(runes) > engineTitleExcerpt {
			content = string(runes[:engineTitleExcerpt])
		}
		builder.WriteString(message.Role + ": " + content + "\n")
		added++
		if added >= 4 {
			break
		}
	}
	return builder.String()
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

// answer builds the prompt from the stored transcript and asks the session model.
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
	return AnswerStream(ctx, ref, llm.ChatRequest{
		Messages: buildPrompt(ctx, conversation, history, attachments),
	}, onDelta)
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
	mu   sync.Mutex
	subs map[int64]map[chan Event]struct{}
}

func newEventBroker() *eventBroker {
	return &eventBroker{subs: make(map[int64]map[chan Event]struct{})}
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
}

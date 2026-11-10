// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/timeutil"
	llm "gitea.dev/services/agent/llm"
	agent_skills "gitea.dev/services/agent/skills"
	agent_tools "gitea.dev/services/agent/tools"
)

const (
	engineWorkers   = setting.DefaultMaxConcurrentSessions
	engineQueueSize = 256
	// engineMaxWorkers caps what an instance may ask for, so one setting cannot overrun it
	engineMaxWorkers   = 64
	engineRunTimeout   = 10 * time.Minute
	engineHistoryLimit = 40
	enginePromptLimit  = 100 * 1024 // cap the transcript handed to the model

	// engineToolRounds caps how often one answer may stop to call tools before it has
	// to answer without them, so a model that keeps calling still finishes.
	engineToolRounds = 6

	// sweepInterval is how often the engine looks for runs nothing is working on, and
	// sweepGrace how long past its deadline a run is given before it is finished: every
	// request ends, even when the worker that held it does not.
	sweepInterval = time.Minute
	sweepGrace    = 2 * time.Minute

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
	// QueuePosition is where a waiting run stands, counting from one, and QueueWaiting is how
	// many runs wait altogether: a reader sees how long the wait is, not only that it waits.
	QueuePosition int `json:"queuePosition,omitempty"`
	QueueWaiting  int `json:"queueWaiting,omitempty"`
	// QueueWaitedSeconds is how long a waiting run has been waiting, and QueueSince is when it
	// was queued: how long a run takes is not knowable, how long this one has waited is.
	QueueWaitedSeconds int   `json:"queueWaitedSeconds,omitempty"`
	QueueSince         int64 `json:"queueSince,omitempty"`
}

// RunStateOf renders a run for the clients watching it.
func RunStateOf(run *agent_model.Run) RunState {
	state := RunState{
		ID:             run.ID,
		ConversationID: run.ConversationID,
		Status:         run.Status,
		Error:          run.Error,
		Cancelable:     agent_model.IsRunActive(run.Status),
	}
	if run.Status == agent_model.RunStatusQueued {
		if position, waiting := engine.queue.position(run.ID); waiting {
			state.QueuePosition, state.QueueWaiting = position, engine.queue.waiting()
			state.QueueSince = run.CreatedUnix.AsTime().Unix()
			state.QueueWaitedSeconds = int(time.Since(run.CreatedUnix.AsTime()).Seconds())
		}
	}
	return state
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
	queue  *runQueue
	broker *eventBroker

	mu        sync.Mutex
	cancels   map[int64]context.CancelFunc // conversation ID -> cancel of the active run
	locks     map[int64]*sync.Mutex        // serializes the runs of one conversation
	approvals map[int64]chan bool          // approval ID -> the waiting run
	waited    map[int64]int64              // run -> the approval its waiting note names
	progress  map[int64]string             // the tool a run last said it was reading with
}

var engine = &Engine{
	queue:     newRunQueue(),
	broker:    newEventBroker(),
	cancels:   make(map[int64]context.CancelFunc),
	locks:     make(map[int64]*sync.Mutex),
	approvals: make(map[int64]chan bool),
	waited:    make(map[int64]int64),
	progress:  make(map[int64]string),
}

// InitEngine recovers the runs a restart interrupted and starts the workers.
func InitEngine(ctx context.Context) error {
	if err := recoverRuns(ctx); err != nil {
		return err
	}
	InitMentions()
	for range engineWorkerCount() {
		go engine.work()
	}
	// a test calls sweepRuns itself, so the ticker would only race the rows it plants
	if !setting.IsInTesting {
		go engine.sweep()
	}
	return nil
}

// recoverRuns fails the runs that were in flight and re-queues the ones that never started.
func recoverRuns(ctx context.Context) error {
	interrupted := make([]*agent_model.Run, 0, 8)
	if err := db.GetEngine(ctx).
		Where("status = ?", agent_model.RunStatusRunning).
		Find(&interrupted); err != nil {
		return err
	}
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
	// a thread that was told the agent is working hears how it ended, or it waits forever
	for _, run := range interrupted {
		settleInterruptedRun(ctx, run)
	}

	// a run that waited for its reader cannot be resumed by the call it waited on, which this
	// process no longer has a reader for: it ends here, and says so where it said it was waiting
	waiting := make([]*agent_model.Run, 0, 8)
	if err := db.GetEngine(ctx).Where("status = ?", agent_model.RunStatusWaiting).Find(&waiting); err != nil {
		return err
	}
	for _, run := range waiting {
		if err := expireApprovalsOf(ctx, run.ID); err != nil {
			return err
		}
		e := engine
		e.finishRun(ctx, run, agent_model.RunStatusFailed, "the run waited for an approval the restart closed")
		settleInterruptedRun(ctx, run)
	}

	// any other call that waited for its reader belongs to a run that died: nobody can answer it
	if _, err := db.GetEngine(ctx).
		Where("status = ?", agent_model.ToolApprovalPending).
		Cols("status", "decided_unix").
		Update(&agent_model.ToolApproval{Status: agent_model.ToolApprovalExpired, DecidedUnix: timeutil.TimeStampNow()}); err != nil {
		return err
	}

	runs := make([]*agent_model.Run, 0, 8)
	if err := db.GetEngine(ctx).Where("status = ?", agent_model.RunStatusQueued).OrderBy("id ASC").Find(&runs); err != nil {
		return err
	}
	for _, run := range runs {
		engine.enqueue(run.ID, runPriorityOf(ctx, run))
	}
	if len(runs) > 0 {
		log.Info("Agent engine: re-queued %d run(s) after restart", len(runs))
	}
	return nil
}

// SubmitUserMessage stores a user message and starts a run for it. The caller does not
// wait for the answer. The stored message is returned so files can be attached to it.
//
// The message and its run are stored together: a message with no run would be a request
// nothing ever answers, and a run with no message would answer something that is not there.
func SubmitUserMessage(ctx context.Context, conversation *agent_model.Conversation, userID int64, content string) (*agent_model.Message, *agent_model.Run, error) {
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: userID, Status: agent_model.RunStatusQueued}
	message := &agent_model.Message{}
	err := db.WithTx(ctx, func(ctx context.Context) error {
		appended, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleUser, content, "")
		if err != nil {
			return err
		}
		message = appended
		return db.Insert(ctx, run)
	})
	if err != nil {
		return nil, nil, err
	}
	engine.broker.publish(conversation.ID, 0, Event{Name: EventMessage, Data: message.ID})
	engine.startQueuedRun(conversation, run)
	return message, run, nil
}

// StartRun queues a new run for a conversation. It is used when a message only needs an
// answer, e.g. after an edit rewound the transcript. userID is who asked for it: the
// tools of the run read only what that user may see.
func StartRun(ctx context.Context, conversation *agent_model.Conversation, userID int64) (*agent_model.Run, error) {
	run, err := newRun(ctx, conversation, userID)
	if err != nil {
		return nil, err
	}
	engine.startQueuedRun(conversation, run)
	return run, nil
}

// newRun stores a queued run in the given context, which may be a caller's transaction, and
// leaves it to the caller to hand it to a worker: a worker that found an uncommitted row
// would drop the run.
func newRun(ctx context.Context, conversation *agent_model.Conversation, userID int64) (*agent_model.Run, error) {
	run := &agent_model.Run{ConversationID: conversation.ID, UserID: userID, Status: agent_model.RunStatusQueued}
	if err := db.Insert(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// startQueuedRun hands a stored run to a worker. It is called after the run is committed:
// a worker that finds no row would drop the run.
func (e *Engine) startQueuedRun(conversation *agent_model.Conversation, run *agent_model.Run) {
	e.enqueue(run.ID, runIsPriority(conversation.Kind))
	e.publishRun(conversation.ID, run)
}

// runIsPriority reports whether a run of that session goes ahead of the queue: the kinds the
// instance named do, so the always-on session keeps the instance looked after while readers wait.
func runIsPriority(kind string) bool {
	kinds := setting.Agent.PriorityKinds
	if len(kinds) == 0 {
		kinds = setting.DefaultPriorityKinds
	}
	return slices.Contains(kinds, kind)
}

// runPriorityOf answers the same question for a run that is already stored.
func runPriorityOf(ctx context.Context, run *agent_model.Run) bool {
	conversation, exist, err := db.GetByID[agent_model.Conversation](ctx, run.ConversationID)
	return err == nil && exist && runIsPriority(conversation.Kind)
}

// EditUserMessageAndRun rewrites a user message and starts the run that re-answers from it,
// in one step: an edit that could not start its run would leave the reader with a request
// nothing answers.
func EditUserMessageAndRun(ctx context.Context, conversation *agent_model.Conversation, userID, messageID int64, content string) (*agent_model.Message, *agent_model.Run, error) {
	run := &agent_model.Run{}
	message := &agent_model.Message{}
	err := db.WithTx(ctx, func(ctx context.Context) error {
		edited, err := EditUserMessage(ctx, conversation, messageID, content)
		if err != nil {
			return err
		}
		message = edited
		started, err := newRun(ctx, conversation, userID)
		if err != nil {
			return err
		}
		run = started
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	engine.startQueuedRun(conversation, run)
	return message, run, nil
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
	// a run that waits for its reader has no worker either: it is finished here, and the call it
	// waited on can no longer be answered
	if run.Status == agent_model.RunStatusWaiting {
		if err := expireApprovalsOf(ctx, run.ID); err != nil {
			return err
		}
	} else if run.Status != agent_model.RunStatusQueued {
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
		In("status", []string{agent_model.RunStatusQueued, agent_model.RunStatusRunning, agent_model.RunStatusWaiting}).
		OrderBy("id DESC").
		Get(run)
	if err != nil || !exist {
		return nil, err
	}
	return run, nil
}

// BusyRuns returns the runs the doer's pages should mark as answering: a page that has just
// connected learns the current state instead of waiting for the next change.
func BusyRuns(ctx context.Context, doer *user_model.User) ([]RunState, error) {
	runs := make([]*agent_model.Run, 0, 8)
	if err := db.GetEngine(ctx).Table("agent_run").
		In("status", []string{agent_model.RunStatusQueued, agent_model.RunStatusRunning, agent_model.RunStatusWaiting}).
		OrderBy("id ASC").Find(&runs); err != nil {
		return nil, fmt.Errorf("find busy runs: %w", err)
	}
	states := make([]RunState, 0, len(runs))
	for _, run := range runs {
		conversation, exist, err := db.GetByID[agent_model.Conversation](ctx, run.ConversationID)
		if err != nil || !exist {
			continue
		}
		if ok, err := CanUseConversation(ctx, doer, conversation); err != nil || !ok {
			continue
		}
		states = append(states, RunStateOf(run))
	}
	return states, nil
}

// ActiveRunStates is every run that is still going, with what the queue says about it: a page
// that has just connected learns that state instead of waiting for the next change.
func ActiveRunStates(ctx context.Context) ([]RunState, error) {
	runs := make([]*agent_model.Run, 0, 16)
	if err := db.GetEngine(ctx).In("status", []string{
		agent_model.RunStatusQueued, agent_model.RunStatusRunning, agent_model.RunStatusWaiting,
	}).OrderBy("id ASC").Find(&runs); err != nil {
		return nil, fmt.Errorf("find active runs: %w", err)
	}
	states := make([]RunState, 0, len(runs))
	for _, run := range runs {
		states = append(states, RunStateOf(run))
	}
	return states, nil
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
	engine.broker.publish(conversation.ID, 0, Event{Name: EventConversation, Data: State(conversation)})
}

// Subscribe attaches to the updates of a conversation. The returned function detaches.
func Subscribe(conversationID int64) (<-chan Event, func()) {
	return engine.broker.subscribe(conversationID)
}

// SubscribeRunEvents attaches to the run changes of one user's conversations.
func SubscribeRunEvents(userID int64) (<-chan Event, func()) {
	return engine.broker.subscribeRuns(userID)
}

// enqueue hands a run to the queue. The queue has no capacity: a run waits for a worker
// however many are already waiting, and says where it stands.
func (e *Engine) enqueue(runID int64, priority bool) {
	e.queue.push(runID, priority)
}

func (e *Engine) work() {
	for {
		e.executeSafely(e.queue.take())
	}
}

// engineWorkerCount is how many sessions the agent answers at once: the instance says so, and a
// request that arrives while that many are working waits in the queue.
func engineWorkerCount() int {
	workers := setting.Agent.MaxConcurrentSessions
	if workers <= 0 {
		workers = engineWorkers
	}
	return min(workers, engineMaxWorkers)
}

// sweepRuns finishes the runs nothing is working on any more, so that a request never rests
// in a state that only a reader could tell from progress:
//
//   - running past its own deadline: the worker that held it is gone
//   - queued for longer than a run may take: it never reached a worker
//   - waiting without a call left to answer: nobody can continue it
//
// A run still inside the deadline, or waiting on a call that is really there, is left alone.
func sweepRuns(ctx context.Context) error {
	staleBefore := timeutil.TimeStampNow() - timeutil.TimeStamp((engineRunTimeout + sweepGrace).Seconds())
	stale := make([]*agent_model.Run, 0, 8)
	if err := db.GetEngine(ctx).Where("status = ?", agent_model.RunStatusRunning).
		And("started_unix > 0").And("started_unix < ?", staleBefore).
		Find(&stale); err != nil {
		return fmt.Errorf("find running runs: %w", err)
	}
	neverStarted := make([]*agent_model.Run, 0, 8)
	if err := db.GetEngine(ctx).Where("status = ?", agent_model.RunStatusQueued).
		And("created_unix < ?", staleBefore).
		Find(&neverStarted); err != nil {
		return fmt.Errorf("find queued runs: %w", err)
	}
	waiting := make([]*agent_model.Run, 0, 8)
	if err := db.GetEngine(ctx).Where("status = ?", agent_model.RunStatusWaiting).
		And("started_unix > 0").And("started_unix < ?", staleBefore).
		Find(&waiting); err != nil {
		return fmt.Errorf("find waiting runs: %w", err)
	}
	for _, run := range stale {
		if finishStrandedRun(ctx, run, "the model did not answer in time") {
			log.Warn("Agent engine: run %d was left running by a lost worker", run.ID)
		}
	}
	for _, run := range neverStarted {
		if finishStrandedRun(ctx, run, "the run was never started") {
			log.Warn("Agent engine: run %d was never handed to a worker", run.ID)
		}
	}
	for _, run := range waiting {
		if finishWaitingRun(ctx, run) {
			log.Warn("Agent engine: run %d waited for something that is gone", run.ID)
		}
	}
	return nil
}

// finishStrandedRun fails a run nothing is working on, holding its conversation while it
// decides: a run a worker is on, or one that moved on since it was read, is left alone.
func finishStrandedRun(ctx context.Context, run *agent_model.Run, reason string) bool {
	lock := engine.conversationLock(run.ConversationID)
	if !lock.TryLock() {
		return false
	}
	defer lock.Unlock()
	reloaded, exist, err := db.GetByID[agent_model.Run](ctx, run.ID)
	if err != nil || !exist || reloaded.Status != run.Status {
		return false
	}
	engine.finishRun(ctx, reloaded, agent_model.RunStatusFailed, reason)
	return true
}

// finishWaitingRun fails a run whose call can no longer be answered: the call is gone, or the
// only reader who could answer it is. It is decided under the conversation, so a reader
// answering at the same moment wins rather than the sweep.
func finishWaitingRun(ctx context.Context, run *agent_model.Run) bool {
	lock := engine.conversationLock(run.ConversationID)
	if !lock.TryLock() {
		return false
	}
	defer lock.Unlock()
	reloaded, exist, err := db.GetByID[agent_model.Run](ctx, run.ID)
	if err != nil || !exist || reloaded.Status != agent_model.RunStatusWaiting {
		return false
	}
	pending, err := PendingApprovals(ctx, reloaded.ConversationID)
	if err != nil {
		log.Error("Agent engine: unable to read the calls of run %d: %v", reloaded.ID, err)
		return false
	}
	if !slices.ContainsFunc(pending, func(approval *agent_model.ToolApproval) bool {
		return approval.RunID == reloaded.ID
	}) {
		engine.finishRun(ctx, reloaded, agent_model.RunStatusFailed, "the call it waited for is gone")
		return true
	}
	// only the reader who asked may answer, so a reader who is gone leaves a run that nothing
	// can continue
	if reloaded.UserID == 0 {
		return false
	}
	if _, err := user_model.GetUserByID(ctx, reloaded.UserID); err != nil {
		if !user_model.IsErrUserNotExist(err) {
			log.Error("Agent engine: unable to read the reader of run %d: %v", reloaded.ID, err)
			return false
		}
		engine.finishRun(ctx, reloaded, agent_model.RunStatusFailed, "the reader who asked is gone")
		return true
	}
	return false
}

// sweep looks for runs nothing is working on, periodically: only a restart used to do that.
func (e *Engine) sweep() {
	for {
		time.Sleep(sweepInterval)
		if err := sweepRuns(context.Background()); err != nil {
			log.Error("Agent engine: sweeping runs failed: %v", err)
		}
	}
}

// executeSafely keeps a run that panicked from resting as one that is still going, and keeps
// the worker: a panic that killed it would leave every request behind it unanswered.
func (e *Engine) executeSafely(runID int64) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error("Agent engine: run %d panicked: %v\n%s", runID, recovered, debug.Stack())
			e.failUnfinishedRun(context.Background(), runID, "the agent stopped unexpectedly")
		}
	}()
	e.execute(runID)
}

// failUnfinishedRun fails a run nothing finished, unless something else already has.
func (e *Engine) failUnfinishedRun(ctx context.Context, runID int64, reason string) {
	run, exist, err := db.GetByID[agent_model.Run](ctx, runID)
	if err != nil || !exist || !agent_model.IsRunActive(run.Status) {
		return
	}
	e.finishRun(ctx, run, agent_model.RunStatusFailed, reason)
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

	result, err := e.answer(runCtx, conversation, run)
	if errors.Is(err, errApprovalWaiting) {
		// the call was put in front of its reader: the run keeps its place and the worker moves on
		return
	}
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

	if strings.TrimSpace(result.Content) == "" {
		e.finishRun(ctx, run, agent_model.RunStatusFailed, "the model returned an empty answer")
		return
	}
	message, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleAssistant, result.Content, result.Reasoning)
	if err != nil {
		e.finishRun(ctx, run, agent_model.RunStatusFailed, err.Error())
		return
	}
	e.broker.publish(conversation.ID, 0, Event{Name: EventMessage, Data: message.ID})

	run.Model = result.Model
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
	e.broker.publish(conversation.ID, 0, Event{Name: EventConversation, Data: State(conversation)})
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

// answerResult is what one run produced: the answer, the reasoning that led to it and the
// model that wrote them.
type answerResult struct {
	Content   string
	Reasoning string
	Model     string
}

// answer builds the prompt from the stored transcript and asks the session model, making
// the tool calls it asks for until it answers.
func (e *Engine) answer(ctx context.Context, conversation *agent_model.Conversation, run *agent_model.Run) (answerResult, error) {
	role := ModelRoleForKind(conversation.Kind)
	ref, err := ModelRoleRef(ctx, role)
	if err != nil {
		return answerResult{}, err
	}
	if ref == "" {
		return answerResult{}, ErrModelRoleNotSet{Role: role}
	}
	history, err := ListMessages(ctx, conversation.ID, engineHistoryLimit)
	if err != nil {
		return answerResult{}, err
	}
	attachments, err := ListMessageAttachments(ctx, messageIDs(history))
	if err != nil {
		return answerResult{}, err
	}
	// every tool an agent calls runs with the agent's own rights, never the asker's: what the
	// agent may read and write is decided once, by the account it works as. An instance that
	// named no such account has no agent to answer with.
	agent := UserOf(ctx)
	if agent == nil {
		return answerResult{}, ErrAgentNotConfigured{}
	}
	session := e.sessionFor(ctx, conversation)
	offerings, err := agent_tools.Available(ctx, session)
	if err != nil {
		return answerResult{}, err
	}
	// everything the model thought during the run, stored with the answer it ends in
	var reasoning strings.Builder
	// every piece goes out at once, so the browser can show the answer while it is written
	onDelta := func(delta llm.Delta) error {
		if err := ctx.Err(); err != nil {
			return err // stop reading when the run was cancelled
		}
		reasoning.WriteString(delta.Reasoning)
		e.broker.publish(conversation.ID, 0, Event{Name: EventDelta, Data: DeltaState{
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
		return answerResult{}, err
	}
	if !slices.ContainsFunc(offerings, func(tool agent_tools.Tool) bool { return tool.Name() == agent_tools.SkillToolName }) {
		catalog = nil
	}
	messages := buildPrompt(ctx, conversation, history, attachments, offerings, catalog)
	// a run that paused on a call its reader answered continues with what that call returned, so
	// the model sees the call it made and the answer to it, however late the reader was
	messages = append(messages, e.resumedCall(ctx, run)...)
	defs := agent_tools.Definitions(offerings)
	var model string // the model that answered, as the gateway reports it
	for round := range engineToolRounds {
		// the last round goes without tools, so the model has to answer with what it read
		askedFor := defs
		if round == engineToolRounds-1 {
			askedFor = nil
		}
		if setting.Agent.PromptTrace {
			writePromptTrace(ctx, run, conversation, round, messages, askedFor)
		}
		message, answered, err := AnswerMessage(ctx, ref, llm.ChatRequest{Messages: messages, Tools: askedFor}, onDelta)
		if err != nil {
			return answerResult{}, err
		}
		model = answered
		calls := message.ToolCalls
		if len(calls) == 0 {
			// a gateway that does not carry calls leaves the model writing them into the answer:
			// the call is a call, and the words around it are what the reader is shown
			calls, message.Content = llm.TextToolCalls(message.Content)
		}
		if len(calls) == 0 {
			return answerResult{Content: message.Content, Reasoning: reasoning.String(), Model: model}, nil
		}
		message.ToolCalls = calls
		// a call has to be named back when its result is returned, or the two cannot be lined
		// up: a gateway may leave a call unnamed, and may name two calls alike
		named := make(map[string]bool, len(message.ToolCalls))
		for i := range message.ToolCalls {
			call := &message.ToolCalls[i]
			if call.ID == "" || named[call.ID] {
				call.ID = fmt.Sprintf("call_%d_%d", round, i)
			}
			named[call.ID] = true
			if call.Type == "" {
				call.Type = "function"
			}
		}
		// the calls are what the reader watches happen; the answer starts after them
		e.broker.publish(conversation.ID, 0, Event{Name: EventDelta, Data: DeltaState{RunID: run.ID, Reset: true}})
		messages = append(messages, *message)
		for _, call := range message.ToolCalls {
			content, err := e.callTool(ctx, conversation, run, agent, call)
			if err != nil {
				return answerResult{}, err
			}
			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: call.ID,
			})
		}
	}
	return answerResult{}, errors.New("the agent kept calling tools without answering")
}

// readerOf is the user a run answers, when it is still known.
func readerOf(ctx context.Context, run *agent_model.Run) *user_model.User {
	if run.UserID == 0 {
		return nil
	}
	user, err := user_model.GetUserByID(ctx, run.UserID)
	if err != nil {
		return nil
	}
	return user
}

// callTool makes one call the model asked for, records it in the transcript and returns
// what the model gets to read.
func (e *Engine) callTool(ctx context.Context, conversation *agent_model.Conversation, run *agent_model.Run, doer *user_model.User, call llm.ToolCall) (string, error) {
	// a tool that takes nothing is called with nothing; a gateway that sends no arguments at
	// all is read as the empty object the tool expects
	if strings.TrimSpace(call.Function.Arguments) == "" {
		call.Function.Arguments = "{}"
	}
	session := e.sessionFor(ctx, conversation)
	tool, decision, err := agent_tools.Check(ctx, session, call.Function.Name)
	switch {
	case err != nil:
		return e.recordTool(ctx, conversation, call, err.Error()), nil
	case decision == agent_tools.DecisionDenied:
		return e.recordTool(ctx, conversation, call, "That tool is switched off for this session."), nil
	case doer == nil:
		return e.recordTool(ctx, conversation, call, "No user belongs to this session, so tools cannot run."), nil
	}
	if decision == agent_tools.DecisionAsk {
		if answer, already := e.settledCall(ctx, run, call); already {
			return answer, nil
		}
		if AutopilotOf(ctx, conversation) {
			// nobody is asked: the run reviews the call itself and goes on
			return e.autopilotCall(ctx, conversation, run, doer, session, call)
		}
		if err := e.awaitApproval(ctx, conversation, run, call); err != nil {
			return "", err
		}
	}
	output, err := runToolSafely(ctx, tool, doer, session, call)
	if err != nil {
		log.Error("Agent engine: tool %s of run %d failed: %v", call.Function.Name, run.ID, err)
		return e.recordTool(ctx, conversation, call, "The tool could not run: "+err.Error()), nil
	}
	return e.recordTool(ctx, conversation, call, output), nil
}

// AutopilotOf reports whether a session decides for itself instead of asking a reader: the
// instance may turn it on for everyone, and a session may turn it on for itself.
func AutopilotOf(_ context.Context, conversation *agent_model.Conversation) bool {
	return setting.Agent.Autopilot || (conversation != nil && conversation.Autopilot)
}

// autopilotCall is a call that would have been put to a reader, decided by the run itself. The
// review answers with its reasoning, the call runs when it says so, and the decision is kept in
// the transcript where a reader can read it back later: work done without a person in the middle
// is work that has to be able to say who decided.
func (e *Engine) autopilotCall(ctx context.Context, conversation *agent_model.Conversation, run *agent_model.Run, doer *user_model.User, session agent_tools.Session, call llm.ToolCall) (string, error) {
	if call.Function.Name == agent_tools.AskUserToolName {
		// a question with nobody to ask is not a question: the run goes on with what it has
		return e.recordTool(ctx, conversation, call, "Autopilot is on, so no reader was asked: decide with what you have and say what you assumed."), nil
	}
	allow, reason := e.reviewCall(ctx, conversation, run, call)
	if !allow {
		return e.recordTool(ctx, conversation, call, "Autopilot refused this call: "+reason), nil
	}
	tool, decision, err := agent_tools.Check(ctx, session, call.Function.Name)
	if err != nil || decision == agent_tools.DecisionDenied {
		return e.recordTool(ctx, conversation, call, "That tool is switched off for this session."), nil
	}
	output, err := runToolSafely(ctx, tool, doer, session, call)
	if err != nil {
		log.Error("Agent engine: tool %s of run %d failed under autopilot: %v", call.Function.Name, run.ID, err)
		return e.recordTool(ctx, conversation, call, "The tool could not run: "+err.Error()), nil
	}
	// the decision is part of what the tool returned, so the transcript keeps it whatever the
	// call was and whoever reads it later
	return e.recordTool(ctx, conversation, call, "(autopilot allowed this call: "+reason+")\n"+output), nil
}

// reviewCall asks the run's own model whether a call should go ahead, and why. A review that
// cannot be read is a refusal: a call nothing vouched for does not run.
func (e *Engine) reviewCall(ctx context.Context, conversation *agent_model.Conversation, run *agent_model.Run, call llm.ToolCall) (bool, string) {
	ref, err := ModelRoleRef(ctx, ModelRoleForKind(conversation.Kind))
	if err != nil || ref == "" {
		return false, "no model is configured to review the call"
	}
	history, err := ListMessages(ctx, conversation.ID, 6)
	if err != nil {
		return false, "the session could not be read"
	}
	var asked strings.Builder
	for _, message := range slices.Backward(history) {
		if message.Role == agent_model.MessageRoleUser {
			asked.WriteString(strings.TrimSpace(message.Content))
			break
		}
	}
	system := "You are the agent working in this session. A call you made is normally put to a " +
		"person before it runs; nobody is available now, so decide for yourself. Allow it only if " +
		"it is what the request needs and nothing in it goes further than that. Answer with one " +
		"JSON object and nothing else: {\"allow\": true or false, \"reason\": \"one line\"}."
	user := "The call: " + call.Function.Name + " with " + call.Function.Arguments +
		"\nWhat was asked of you: " + asked.String() +
		"\nThe session: " + conversation.Title + " (" + conversation.Kind + ")"
	reviewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), engineTitleTimeout)
	defer cancel()
	answer, _, err := Answer(reviewCtx, ref, llm.ChatRequest{Messages: []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}})
	if err != nil {
		log.Warn("Agent engine: autopilot review of run %d failed: %v", run.ID, err)
		return false, "the review could not be read"
	}
	var verdict struct {
		Allow  bool   `json:"allow"`
		Reason string `json:"reason"`
	}
	start := strings.Index(answer, "{")
	if start < 0 || json.Unmarshal([]byte(answer[start:strings.LastIndex(answer, "}")+1]), &verdict) != nil {
		return false, "the review did not say whether to go on"
	}
	reason := strings.TrimSpace(verdict.Reason)
	if reason == "" {
		reason = "it is what the request needs"
	}
	return verdict.Allow, reason
}

// resumedCall is the call a paused run continues from, and it is read once: the approval keeps
// what the call returned until the run that asked for it has it back.
func (e *Engine) resumedCall(ctx context.Context, run *agent_model.Run) []llm.Message {
	approval := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).
		Where("run_id = ?", run.ID).And("result <> ?", "").
		OrderBy("id DESC").Get(approval)
	if err != nil || !has {
		return nil
	}
	id := fmt.Sprintf("call_%d", approval.ID)
	return []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: id, Type: "function",
			Function: llm.FunctionCall{Name: approval.Tool, Arguments: approval.Arguments},
		}}},
		{Role: "tool", ToolCallID: id, Content: approval.Result},
	}
}

// settledCall is the outcome of a call this run already had answered: a resumed model that asks
// for the same thing again is reading its own question, not asking a new one.
func (e *Engine) settledCall(ctx context.Context, run *agent_model.Run, call llm.ToolCall) (string, bool) {
	approval := &agent_model.ToolApproval{}
	has, err := db.GetEngine(ctx).
		Where("run_id = ?", run.ID).
		And("tool = ?", call.Function.Name).
		And("arguments = ?", call.Function.Arguments).
		And("status = ?", agent_model.ToolApprovalAllowed).
		OrderBy("id DESC").Get(approval)
	if err != nil || !has || approval.Result == "" {
		return "", false
	}
	return approval.Result, true
}

// sessionFor is the session a run works in. A session the agent owns is one it answers for on a
// surface: its reads stay with the asking user, and its writes carry the agent's own name.
func (e *Engine) sessionFor(ctx context.Context, conversation *agent_model.Conversation) agent_tools.Session {
	session := agent_tools.Session{Kind: conversation.Kind, RepoID: conversation.RepoID}
	if agent := UserOf(ctx); agent != nil && conversation.OwnerID == agent.ID {
		session.ActorID = agent.ID
	}
	return session
}

// runToolSafely runs one tool, turning a panic into the failure it is: a tool is written by
// hand, and the instance must survive a bad one.
func runToolSafely(ctx context.Context, tool agent_tools.Tool, doer *user_model.User, session agent_tools.Session, call llm.ToolCall) (output string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error("Agent engine: tool %s of run %d panicked: %v", call.Function.Name, session.RepoID, recovered)
			err = fmt.Errorf("the tool failed while it ran (%v)", recovered)
		}
	}()
	return tool.Run(ctx, doer, session, call.Function.Arguments)
}

// recordTool puts one call and its result in the transcript, and hands the same result
// back for the model to read.
func (e *Engine) recordTool(ctx context.Context, conversation *agent_model.Conversation, call llm.ToolCall, result string) string {
	if message, err := AppendMessage(ctx, conversation.ID, agent_model.MessageRoleTool, argsLine(call.Function.Name, call.Function.Arguments)+"\n"+result, ""); err == nil {
		e.broker.publish(conversation.ID, 0, Event{Name: EventMessage, Data: message.ID})
	} else {
		log.Error("Agent engine: unable to record tool %s: %v", call.Function.Name, err)
	}
	return result
}

// engineArgsLimit caps the arguments one record keeps: the model can put a document in them,
// and the record is read by a person.
const engineArgsLimit = 2 * 1024

// argsLine renders a call the way the transcript shows it.
func argsLine(name, args string) string {
	args = strings.Join(strings.Fields(args), " ")
	if args == "" || args == "{}" {
		return name
	}
	if len(args) > engineArgsLimit {
		args = args[:engineArgsLimit] + "…"
	}
	return name + " " + args
}

// errApprovalWaiting stops the answer loop of a run that paused on a call its reader has to
// allow. The run keeps its place: the worker is free, and the call continues whenever the reader
// comes back.
var errApprovalWaiting = errors.New("the run waits for its reader")

// awaitApproval puts a manual call in front of the reader. It does not wait: a reader may come
// back an hour later, so the run is put down and picked up again when the call is answered.
func (e *Engine) awaitApproval(ctx context.Context, conversation *agent_model.Conversation, run *agent_model.Run, call llm.ToolCall) error {
	kind := agent_model.ToolApprovalKindApproval
	if call.Function.Name == agent_tools.AskUserToolName {
		// a question is answered with words, so it is asked as one
		kind = agent_model.ToolApprovalKindQuestion
	}
	approval := &agent_model.ToolApproval{
		ConversationID: conversation.ID,
		RunID:          run.ID,
		Tool:           call.Function.Name,
		Arguments:      call.Function.Arguments,
		Kind:           kind,
		Status:         agent_model.ToolApprovalPending,
	}
	if err := db.Insert(ctx, approval); err != nil {
		log.Error("Agent engine: unable to store tool approval: %v", err)
		return fmt.Errorf("the call could not be put to the user: %w", err)
	}
	e.broker.publish(conversation.ID, 0, Event{Name: EventApproval, Data: approval})

	if _, err := db.GetEngine(ctx).ID(run.ID).Cols("status").
		Update(&agent_model.Run{Status: agent_model.RunStatusWaiting}); err != nil {
		return err
	}
	run.Status = agent_model.RunStatusWaiting
	e.publishRun(conversation.ID, run)
	return errApprovalWaiting
}

// expireApprovalsOf closes the calls of a run that ended: nobody can answer them now.
func expireApprovalsOf(ctx context.Context, runID int64) error {
	_, err := db.GetEngine(ctx).Where("run_id = ?", runID).And("status = ?", agent_model.ToolApprovalPending).
		Cols("status", "decided_unix").
		Update(&agent_model.ToolApproval{Status: agent_model.ToolApprovalExpired, DecidedUnix: timeutil.TimeStampNow()})
	return err
}

// ErrApprovalAnswerEmpty is returned when an answer arrives with nothing in it.
type ErrApprovalAnswerEmpty struct{}

func (ErrApprovalAnswerEmpty) Error() string { return "an answer is needed to go on" }

// Is reports whether target is an ErrApprovalAnswerEmpty.
func (err ErrApprovalAnswerEmpty) Is(target error) bool {
	_, ok := target.(ErrApprovalAnswerEmpty)
	return ok
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

// CanAnswerRun reports whether a reader may answer a run: the person whose question started it,
// or a person who may write the repository its session works in. A run nobody owns waits for
// whoever gets there first.
func CanAnswerRun(ctx context.Context, doer *user_model.User, run *agent_model.Run) (bool, error) {
	if doer == nil {
		return false, nil
	}
	if run.UserID == 0 || run.UserID == doer.ID {
		return true, nil
	}
	conversation, exist, err := db.GetByID[agent_model.Conversation](ctx, run.ConversationID)
	if err != nil || !exist || conversation.RepoID == 0 {
		return false, err
	}
	repo, err := repo_model.GetRepositoryByID(ctx, conversation.RepoID)
	if err != nil {
		return false, err
	}
	return CanUseRepository(ctx, doer, repo)
}

// ResolveApproval records what the reader decided about a pending call and wakes the run
// waiting for it. A call left pending by a restart is still closed here.
func ResolveApproval(ctx context.Context, conversationID, approvalID int64, doer *user_model.User, allowed bool) error {
	// the sweep of stranded runs decides under the same conversation, so a reader answering a
	// call and the sweep closing it are never both right
	lock := engine.conversationLock(conversationID)
	lock.Lock()
	defer lock.Unlock()

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
	if !exist {
		return ErrApprovalNotPending{}
	}
	may, err := CanAnswerRun(ctx, doer, run)
	if err != nil {
		return err
	}
	if !may {
		return ErrApprovalNotOwner{}
	}
	status := agent_model.ToolApprovalRefused
	if allowed {
		status = agent_model.ToolApprovalAllowed
	}
	result := "The user did not allow that call."
	if allowed {
		result = engine.runApprovedCall(ctx, conversationID, run, approval)
	}
	return settleRequest(ctx, conversationID, run, approval, status, doer.ID, result, "")
}

// AnswerQuestion records what a reader wrote back to a question and wakes the run waiting for it.
func AnswerQuestion(ctx context.Context, conversationID, approvalID int64, doer *user_model.User, answer string) error {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return ErrApprovalAnswerEmpty{}
	}
	lock := engine.conversationLock(conversationID)
	lock.Lock()
	defer lock.Unlock()

	request := &agent_model.ToolApproval{}
	exist, err := db.GetEngine(ctx).ID(approvalID).
		And("conversation_id = ?", conversationID).
		Get(request)
	if err != nil {
		return err
	}
	if !exist || request.Status != agent_model.ToolApprovalPending || !request.IsQuestion() {
		return ErrApprovalNotPending{}
	}
	run, exist, err := db.GetByID[agent_model.Run](ctx, request.RunID)
	if err != nil {
		return err
	}
	if !exist {
		return ErrApprovalNotPending{}
	}
	may, err := CanAnswerRun(ctx, doer, run)
	if err != nil {
		return err
	}
	if !may {
		return ErrApprovalNotOwner{}
	}
	// the answer is what the call returned, and the reader's words are kept as they were written
	return settleRequest(ctx, conversationID, run, request, agent_model.ToolApprovalAllowed, doer.ID, "The reader answered: "+answer, answer)
}

// settleRequest closes one request and lets the run go on: a worker is not held while a reader
// thinks, so the run takes its place in the queue again.
func settleRequest(ctx context.Context, conversationID int64, run *agent_model.Run, request *agent_model.ToolApproval, status string, decidedBy int64, result, answer string) error {
	if _, err := db.GetEngine(ctx).ID(request.ID).Cols("status", "decided_user_id", "decided_unix", "result", "answer").
		Update(&agent_model.ToolApproval{
			Status: status, DecidedUserID: decidedBy, Result: result, Answer: answer,
			DecidedUnix: timeutil.TimeStampNow(),
		}); err != nil {
		return err
	}
	request.Status, request.DecidedUserID, request.Result, request.Answer = status, decidedBy, result, answer
	engine.broker.publish(conversationID, 0, Event{Name: EventApproval, Data: request})
	if _, err := db.GetEngine(ctx).ID(run.ID).Cols("status").
		Where("status = ?", agent_model.RunStatusWaiting).
		Update(&agent_model.Run{Status: agent_model.RunStatusQueued}); err != nil {
		return err
	}
	engine.enqueue(run.ID, runPriorityOf(ctx, run))
	return nil
}

// runApprovedCall runs the call a reader allowed, as the run's own account, and keeps what it
// returned for the resumed run to read.
func (e *Engine) runApprovedCall(ctx context.Context, conversationID int64, run *agent_model.Run, approval *agent_model.ToolApproval) string {
	conversation, exist, err := db.GetByID[agent_model.Conversation](ctx, conversationID)
	if err != nil || !exist {
		return "The session no longer exists."
	}
	doer := UserOf(ctx)
	if doer == nil {
		return "No agent account is configured, so tools cannot run."
	}
	session := e.sessionFor(ctx, conversation)
	tool, decision, err := agent_tools.Check(ctx, session, approval.Tool)
	if err != nil || decision == agent_tools.DecisionDenied {
		return "That tool is switched off for this session."
	}
	call := llm.ToolCall{
		ID: fmt.Sprintf("call_%d", approval.ID), Type: "function",
		Function: llm.FunctionCall{Name: approval.Tool, Arguments: approval.Arguments},
	}
	output, err := runToolSafely(ctx, tool, doer, session, call)
	if err != nil {
		log.Error("Agent engine: tool %s of run %d failed: %v", approval.Tool, run.ID, err)
		return e.recordTool(ctx, conversation, call, "The tool could not run: "+err.Error())
	}
	return e.recordTool(ctx, conversation, call, output)
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
	// a run that is over has no call a reader could still answer: leaving one pending would let
	// an answer run a write for a request that already ended
	if err := expireApprovalsOf(ctx, run.ID); err != nil {
		log.Error("Agent engine: unable to close the calls of run %d: %v", run.ID, err)
	}
	// what the run said in its thread is history, so the notes it took are its own
	e.mu.Lock()
	delete(e.waited, run.ID)
	delete(e.progress, run.ID)
	e.mu.Unlock()
	e.publishRun(run.ConversationID, run)
}

func (e *Engine) publishRun(conversationID int64, run *agent_model.Run) {
	e.broker.publish(conversationID, run.UserID, Event{Name: EventRun, Data: RunStateOf(run)})
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
	mu sync.Mutex
	// subs are the watchers of one conversation: its transcript and its own run.
	subs map[int64]map[chan Event]struct{}
	// runs are the watchers of the run changes of one user's conversations, so a page
	// can mark the sessions that are answering without seeing anybody else's.
	runs map[int64]map[chan Event]struct{}
	// all are the watchers of every run change: the page that monitors the instance.
	all map[chan Event]struct{}
}

func newEventBroker() *eventBroker {
	return &eventBroker{
		subs: make(map[int64]map[chan Event]struct{}),
		all:  make(map[chan Event]struct{}),
		runs: make(map[int64]map[chan Event]struct{}),
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
// owner is the user whose page follows the run changes of every one of its conversations;
// zero when the caller has no owner to hand them to.
func (b *eventBroker) publish(conversationID, owner int64, event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[conversationID] {
		select {
		case ch <- event:
		default:
		}
	}
	if event.Name != EventRun || owner == 0 {
		return
	}
	for ch := range b.runs[owner] {
		select {
		case ch <- event:
		default:
		}
	}
	for ch := range b.all {
		select {
		case ch <- event:
		default:
		}
	}
}

// subscribeAll listens to every run change, for the page that watches the instance.
func (b *eventBroker) subscribeAll() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.all[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.all[ch]; ok {
			delete(b.all, ch)
			close(ch)
		}
	}
}

// SubscribeAllRunEvents listens to every run change; the pages that use it check for themselves
// that their reader may watch the instance.
func SubscribeAllRunEvents() (<-chan Event, func()) {
	return engine.broker.subscribeAll()
}

// subscribeRuns listens to the run changes of one user's conversations.
func (b *eventBroker) subscribeRuns(userID int64) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	if b.runs[userID] == nil {
		b.runs[userID] = make(map[chan Event]struct{})
	}
	b.runs[userID][ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.runs[userID][ch]; ok {
			delete(b.runs[userID], ch)
			close(ch)
			if len(b.runs[userID]) == 0 {
				delete(b.runs, userID)
			}
		}
	}
}

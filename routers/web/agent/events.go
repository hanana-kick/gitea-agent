// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/templates"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

const (
	// eventsHeartbeat keeps the connection alive through proxies.
	eventsHeartbeat = 20 * time.Second
	// eventsReplay caps how much of the transcript is replayed to a returning client.
	eventsReplay = 200
)

// attachmentEvent is one file sent with a message.
type attachmentEvent struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	SizeText string `json:"sizeText"`
	URL      string `json:"url"`
	// IsPicture says the transcript may show the file itself.
	IsPicture bool `json:"isPicture"`
}

// toolEvent is one tool record split into what the model called and what came back, so the
// transcript can fold it into one summary line.
type toolEvent struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Result string `json:"result"`
}

// messageEvent is one transcript entry as the browser receives it. The raw content is kept
// for copying, the rendered html is what the transcript shows.
type messageEvent struct {
	ID          int64             `json:"id"`
	Role        string            `json:"role"`
	RoleLabel   string            `json:"roleLabel"`
	Content     string            `json:"content"`
	HTML        string            `json:"html"`
	TimeHTML    string            `json:"timeHTML"`
	Reasoning   string            `json:"reasoning,omitempty"`
	Attachments []attachmentEvent `json:"attachments,omitempty"`
	Tool        *toolEvent        `json:"tool,omitempty"`
}

// approvalEvent is one tool call waiting for the reader to allow it.
type approvalEvent struct {
	ID        int64  `json:"id"`
	RunID     int64  `json:"runID"`
	Tool      string `json:"tool"`
	Arguments string `json:"arguments"`
}

// ChatEvents streams the updates of one session: transcript entries and run state. A client
// may drop at any time; the run continues on the server and the transcript is stored.
func ChatEvents(ctx *context.Context) {
	conversation, err := agent_service.GetConversation(ctx, ctx.Doer, ctx.PathParamInt64("id"))
	if err != nil {
		chatError(ctx, err)
		return
	}

	flusher, ok := ctx.Resp.(http.Flusher)
	if !ok {
		ctx.ServerError("agent events", errors.New("the response writer does not support streaming"))
		return
	}
	header := ctx.Resp.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	// nginx and friends must not buffer an event stream
	header.Set("X-Accel-Buffering", "no")
	ctx.Resp.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, unsubscribe := agent_service.Subscribe(conversation.ID)
	defer unsubscribe()
	// one stream carries both what this conversation does and the run changes of the
	// reader's other conversations, so the session list beside it stays live
	allRuns, unsubscribeAll := agent_service.SubscribeRunEvents(ctx.Doer.ID)
	defer unsubscribeAll()

	// catch the client up on what it missed, then report the current run
	replayEvents(ctx, conversation, eventsSince(ctx))
	if err := writeEvent(ctx, agent_service.EventRun, agent_service.RunStateOf(latestRunState(ctx, conversation.ID))); err != nil {
		return
	}
	// the other sessions of the list are reported once, so a client that just connected
	// marks them without waiting for their next change
	if err := writeBusyRuns(ctx, conversation.ID); err != nil {
		return
	}
	// a client that comes back finds the calls still waiting for its answer
	approvals, err := agent_service.PendingApprovals(ctx, conversation.ID)
	if err != nil {
		log.Error("agent events: unable to list pending tool calls of session %d: %v", conversation.ID, err)
	}
	for _, approval := range approvals {
		if err := writeEvent(ctx, agent_service.EventApproval, approval); err != nil {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(eventsHeartbeat)
	defer heartbeat.Stop()

	clientGone := ctx.Req.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(ctx.Resp, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := writeEvent(ctx, event.Name, event.Data); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-allRuns:
			if !ok {
				return
			}
			// this conversation's own run already came down its own channel
			if state, ok := event.Data.(agent_service.RunState); ok && state.ConversationID == conversation.ID {
				continue
			}
			if err := writeEvent(ctx, event.Name, event.Data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ChatEventsAll streams the run changes of the reader's conversations, so a page without an
// open conversation still marks the sessions that are answering.
func ChatEventsAll(ctx *context.Context) {
	flusher, ok := ctx.Resp.(http.Flusher)
	if !ok {
		ctx.ServerError("agent events", errors.New("the response writer does not support streaming"))
		return
	}
	header := ctx.Resp.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	ctx.Resp.WriteHeader(http.StatusOK)
	flusher.Flush()

	runs, unsubscribe := agent_service.SubscribeRunEvents(ctx.Doer.ID)
	defer unsubscribe()

	// what is answering right now, so a client that just connected marks it without
	// waiting for the next change
	if err := writeBusyRuns(ctx, 0); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(eventsHeartbeat)
	defer heartbeat.Stop()

	clientGone := ctx.Req.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(ctx.Resp, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-runs:
			if !ok {
				return
			}
			if err := writeEvent(ctx, event.Name, event.Data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeBusyRuns reports the runs of the reader's conversations that are answering, leaving
// out the conversation a chat stream already reported.
func writeBusyRuns(ctx *context.Context, skipConversationID int64) error {
	busy, err := agent_service.BusyRuns(ctx, ctx.Doer)
	if err != nil {
		log.Error("agent events: unable to list busy runs: %v", err)
		return nil
	}
	for _, state := range busy {
		if state.ConversationID == skipConversationID {
			continue
		}
		if err := writeEvent(ctx, agent_service.EventRun, state); err != nil {
			return err
		}
	}
	return nil
}

// eventsSince reads the resume cursor: the query parameter wins over Last-Event-ID.
func eventsSince(ctx *context.Context) int64 {
	if raw := ctx.FormString("since"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			return id
		}
	}
	if raw := ctx.Req.Header.Get("Last-Event-ID"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			return id
		}
	}
	return 0
}

// replayEvents sends the messages newer than the client's cursor.
func replayEvents(ctx *context.Context, conversation *agent_model.Conversation, since int64) {
	messages, err := agent_service.ListMessages(ctx, conversation.ID, eventsReplay)
	if err != nil {
		ctx.ServerError("ListMessages", err)
		return
	}
	var missed []*agent_model.Message
	for _, message := range messages {
		if message.ID > since {
			missed = append(missed, message)
		}
	}
	if len(missed) > eventsReplay {
		missed = missed[len(missed)-eventsReplay:]
	}
	for _, message := range missed {
		if _, err := fmt.Fprint(ctx.Resp, formatEvent(agent_service.EventMessage, messageEventOf(ctx, message), message.ID)); err != nil {
			return
		}
	}
}

// writeEvent resolves the payload of an event and writes it. Messages carry their ID as the
// event ID so a reconnecting client can resume from it.
func writeEvent(ctx *context.Context, name string, data any) error {
	eventID := int64(0)
	payload := data
	switch name {
	case agent_service.EventMessage:
		messageID, ok := data.(int64)
		if !ok {
			return nil
		}
		message, err := agent_service.GetMessage(ctx, messageID)
		if err != nil || message == nil {
			return err
		}
		payload = messageEventOf(ctx, message)
		eventID = message.ID
	case agent_service.EventApproval:
		approval, ok := data.(*agent_model.ToolApproval)
		if !ok {
			return nil
		}
		payload = approvalEvent{ID: approval.ID, RunID: approval.RunID, Tool: approval.Tool, Arguments: approval.Arguments}
	}
	_, err := fmt.Fprint(ctx.Resp, formatEvent(name, payload, eventID))
	return err
}

func formatEvent(name string, data any, eventID int64) string {
	body, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	if eventID > 0 {
		return fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", eventID, name, body)
	}
	return fmt.Sprintf("event: %s\ndata: %s\n\n", name, body)
}

func messageEventOf(ctx *context.Context, message *agent_model.Message) messageEvent {
	attachments, err := agent_service.ListMessageAttachments(ctx, []int64{message.ID})
	if err != nil {
		ctx.ServerError("ListMessageAttachments", err)
		return messageEvent{}
	}
	event := messageEvent{
		Attachments: attachmentPayloads(attachments[message.ID]),
		ID:          message.ID,
		Role:        message.Role,
		RoleLabel:   messageRoleLabel(ctx, message.Role),
		Content:     message.Content,
		HTML:        string(templates.NewRenderUtils(ctx).MarkdownToHtml(message.Content)),
		TimeHTML:    string(templates.TimeSince(message.CreatedUnix)),
		Reasoning:   message.Reasoning,
	}
	if message.Role == agent_model.MessageRoleTool {
		// a tool record folds into one summary line: the call, and what came back behind it
		event.Tool = toolRecordOf(message.Content)
	}
	return event
}

// toolRecordOf splits a stored tool record: the call the model made, and what came back.
func toolRecordOf(content string) *toolEvent {
	call, result, _ := strings.Cut(content, "\n")
	name, args, _ := strings.Cut(call, " ")
	return &toolEvent{Name: name, Args: args, Result: strings.TrimSpace(result)}
}

func messageRoleLabel(ctx *context.Context, role string) string {
	switch role {
	case agent_model.MessageRoleUser:
		return string(ctx.Tr("agent.chat_you"))
	case agent_model.MessageRoleAssistant:
		return string(ctx.Tr("agent.chat_agent"))
	case agent_model.MessageRoleTool:
		return string(ctx.Tr("agent.chat_tool"))
	}
	return string(ctx.Tr("agent.chat_system"))
}

// latestRunState returns the current run of a session, or a finished placeholder when the
// session never ran anything.
func latestRunState(ctx *context.Context, conversationID int64) *agent_model.Run {
	run, err := agent_service.LatestRun(ctx, conversationID)
	if err != nil {
		ctx.ServerError("LatestRun", err)
		return &agent_model.Run{Status: agent_model.RunStatusSucceeded}
	}
	if run == nil {
		return &agent_model.Run{Status: agent_model.RunStatusSucceeded}
	}
	return run
}

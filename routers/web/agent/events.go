// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	agent_model "gitea.dev/models/agent"
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
	Attachments []attachmentEvent `json:"attachments,omitempty"`
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
		ctx.ServerError("agent events", fmt.Errorf("the response writer does not support streaming"))
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

	// catch the client up on what it missed, then report the current run
	replayEvents(ctx, conversation, eventsSince(ctx))
	writeEvent(ctx, flusher, agent_service.EventRun, agent_service.RunStateOf(latestRunState(ctx, conversation.ID)))
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
			if err := writeEvent(ctx, flusher, event.Name, event.Data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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
		if _, err := fmt.Fprint(ctx.Resp, formatEvent(ctx, agent_service.EventMessage, messageEventOf(ctx, message), message.ID)); err != nil {
			return
		}
	}
}

// writeEvent resolves the payload of an event and writes it. Messages carry their ID as the
// event ID so a reconnecting client can resume from it.
func writeEvent(ctx *context.Context, flusher http.Flusher, name string, data any) error {
	eventID := int64(0)
	payload := data
	if name == agent_service.EventMessage {
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
	}
	_, err := fmt.Fprint(ctx.Resp, formatEvent(ctx, name, payload, eventID))
	return err
}

func formatEvent(ctx *context.Context, name string, data any, eventID int64) string {
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
	return messageEvent{
		Attachments: attachmentPayloads(attachments[message.ID]),
		ID:          message.ID,
		Role:        message.Role,
		RoleLabel:   messageRoleLabel(ctx, message.Role),
		Content:     message.Content,
		HTML:        string(templates.NewRenderUtils(ctx).MarkdownToHtml(message.Content)),
		TimeHTML:    string(templates.TimeSince(message.CreatedUnix)),
	}
}

func messageRoleLabel(ctx *context.Context, role string) string {
	switch role {
	case agent_model.MessageRoleUser:
		return string(ctx.Tr("agent.chat_you"))
	case agent_model.MessageRoleAssistant:
		return string(ctx.Tr("agent.chat_agent"))
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

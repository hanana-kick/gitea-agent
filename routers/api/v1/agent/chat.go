// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	agent_model "gitea.dev/models/agent"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/json"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/web"
	agent_service "gitea.dev/services/agent"
	"gitea.dev/services/context"
)

// The agent's chat, as a client drives it: a session is opened, a message starts a run, and what
// the run does is read back — the transcript it writes, the call it waits for, the stream it
// reports on. Everything here is the doer's own: a session is read and driven by whoever may open
// it, which is the same rule the chat page follows.

// toSessionResponse renders one session, with the newest run and what waits for a reader.
func toSessionResponse(ctx *context.APIContext, session *agent_model.Conversation, repo *repo_model.Repository) *api.AgentSession {
	response := &api.AgentSession{
		ID: session.ID, Kind: session.Kind, Title: session.Title, Summary: session.Summary,
		RepoID: session.RepoID, Autopilot: session.Autopilot, Icon: session.Icon, FolderID: session.FolderID,
		CreatedAt: session.CreatedUnix.AsTime(), UpdatedAt: session.UpdatedUnix.AsTime(),
	}
	if repo != nil {
		response.Repository = repo.FullName()
	}
	if run, err := agent_service.LatestRun(ctx, session.ID); err == nil && run != nil {
		response.Run = toRunResponse(run)
	}
	if approvals, err := agent_service.PendingApprovals(ctx, session.ID); err == nil {
		response.PendingApprovals = int64(len(approvals))
	}
	return response
}

// toRunResponse renders a run for a client.
func toRunResponse(run *agent_model.Run) *api.AgentRun {
	state := agent_service.RunStateOf(run)
	response := &api.AgentRun{
		ID: state.ID, Status: state.Status, Model: run.Model, Error: run.Error, Cancelable: state.Cancelable,
		QueuePosition: state.QueuePosition, QueueWaiting: state.QueueWaiting,
	}
	if run.StartedUnix > 0 {
		response.StartedAt = run.StartedUnix.AsTime()
	}
	if run.FinishedUnix > 0 {
		response.FinishedAt = run.FinishedUnix.AsTime()
	}
	return response
}

// toMessageResponse renders one message of a transcript.
func toMessageResponse(message *agent_model.Message) *api.AgentMessage {
	response := &api.AgentMessage{
		ID: message.ID, Role: message.Role, Content: message.Content, Reasoning: message.Reasoning,
		CreatedAt: message.CreatedUnix.AsTime(),
	}
	if message.EditedUnix > 0 {
		response.EditedAt = message.EditedUnix.AsTime()
	}
	return response
}

// toApprovalResponse renders a call that waits for a reader.
func toApprovalResponse(approval *agent_model.ToolApproval) *api.AgentApproval {
	return &api.AgentApproval{
		ID: approval.ID, Tool: approval.Tool, Kind: approval.Kind, Arguments: approval.Arguments,
		CreatedAt: approval.CreatedUnix.AsTime(),
	}
}

// sessionOf reads the session a request names, refusing one the doer may not open.
func sessionOf(ctx *context.APIContext) (*agent_model.Conversation, bool) {
	id, err := strconv.ParseInt(ctx.PathParam("id"), 10, 64)
	if err != nil || id <= 0 {
		ctx.APIError(http.StatusBadRequest, "the session id is not a number")
		return nil, false
	}
	session, err := agent_service.GetConversation(ctx, ctx.Doer, id)
	if err != nil {
		switch {
		case errors.Is(err, agent_service.ErrConversationNotFound{}), errors.Is(err, agent_service.ErrConversationDenied{}):
			// a session the doer may not use is not told apart from one that is not there
			ctx.APIError(http.StatusNotFound, "session not found")
		default:
			ctx.APIErrorInternal(err)
		}
		return nil, false
	}
	return session, true
}

// repositoryOfSession reads the repository a session works on, when it works on one.
func repositoryOfSession(ctx *context.APIContext, session *agent_model.Conversation) *repo_model.Repository {
	if session.RepoID <= 0 {
		return nil
	}
	repo, err := repo_model.GetRepositoryByID(ctx, session.RepoID)
	if err != nil {
		return nil
	}
	return repo
}

// ListSessions lists the sessions the doer may open. It is what a client shows first.
func ListSessions(ctx *context.APIContext) {
	// swagger:operation GET /agent/sessions agent agentListSessions
	// ---
	// summary: List the agent sessions of the current user
	// description: Lists every session the user may open — the resident session, the instance sessions and the repository ones.
	// produces:
	// - application/json
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentSessionList"
	list, err := agent_service.ListConversations(ctx, ctx.Doer)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	sessions := make([]*api.AgentSession, 0, 8)
	add := func(session *agent_model.Conversation, repo *repo_model.Repository) {
		if session == nil {
			return
		}
		sessions = append(sessions, toSessionResponse(ctx, session, repo))
	}
	add(list.Resident, nil)
	for _, session := range list.Servers {
		add(session, nil)
	}
	for _, folder := range list.Folders {
		for _, session := range folder.Sessions {
			add(session, nil)
		}
	}
	for _, entry := range list.Repositories {
		add(entry.Session, entry.Repo)
	}
	ctx.JSON(http.StatusOK, api.AgentSessionList{List: sessions, Total: int64(len(sessions))})
}

// CreateSession opens a session, and starts its first run when a message comes with it.
func CreateSession(ctx *context.APIContext) {
	// swagger:operation POST /agent/sessions agent agentCreateSession
	// ---
	// summary: Open an agent session
	// description: Opens a session of the given kind. A repository session is opened on a repository the user may drive the agent on; a server session is for administrators. A message given with it starts the first run.
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/CreateAgentSessionOption"
	// responses:
	//   "201":
	//     "$ref": "#/responses/AgentSession"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "422":
	//     "$ref": "#/responses/validationError"
	form := web.GetForm[*api.CreateAgentSessionOption](ctx)
	switch form.Kind {
	case agent_model.ConversationKindRepository:
		if form.RepoID <= 0 {
			ctx.APIError(http.StatusBadRequest, "repo_id is required for a repository session")
			return
		}
		repo, err := repo_model.GetRepositoryByID(ctx, form.RepoID)
		if err != nil || repo == nil {
			ctx.APIError(http.StatusNotFound, "repository not found")
			return
		}
		allowed, err := agent_service.CanUseRepository(ctx, ctx.Doer, repo)
		if err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		if !allowed {
			ctx.APIError(http.StatusForbidden, "the agent may not be driven on this repository")
			return
		}
		session, err := agent_service.CreateConversation(ctx, agent_model.ConversationKindRepository, ctx.Doer.ID, repo.ID, "")
		if err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		created(ctx, session, repo, form)
	case agent_model.ConversationKindServer:
		if !ctx.Doer.IsAdmin {
			ctx.APIError(http.StatusForbidden, "a server session is for administrators")
			return
		}
		session, err := agent_service.CreateConversation(ctx, agent_model.ConversationKindServer, ctx.Doer.ID, 0, "")
		if err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		created(ctx, session, nil, form)
	default:
		ctx.APIError(http.StatusBadRequest, "kind must be repository or server")
	}
}

// created answers a request that opened a session, sending the first message when one came.
func created(ctx *context.APIContext, session *agent_model.Conversation, repo *repo_model.Repository, form *api.CreateAgentSessionOption) {
	if form.Autopilot != nil {
		if err := agent_service.SetConversationAutopilot(ctx, session, *form.Autopilot); err != nil {
			ctx.APIErrorInternal(err)
			return
		}
	}
	if content := strings.TrimSpace(form.Content); content != "" {
		if _, _, err := agent_service.SubmitUserMessage(ctx, session, ctx.Doer.ID, content); err != nil {
			ctx.APIError(http.StatusBadRequest, err.Error())
			return
		}
	}
	ctx.JSON(http.StatusCreated, toSessionResponse(ctx, session, repo))
}

// GetSession reads one session, with the newest run and what waits for a reader.
func GetSession(ctx *context.APIContext) {
	// swagger:operation GET /agent/sessions/{id} agent agentGetSession
	// ---
	// summary: Get an agent session
	// description: Reads one session — what it works on, whether it decides for itself, its newest run and the calls waiting for a reader.
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentSession"
	//   "404":
	//     "$ref": "#/responses/notFound"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	ctx.JSON(http.StatusOK, toSessionResponse(ctx, session, repositoryOfSession(ctx, session)))
}

// DeleteSession removes a session with its transcript and its runs.
func DeleteSession(ctx *context.APIContext) {
	// swagger:operation DELETE /agent/sessions/{id} agent agentDeleteSession
	// ---
	// summary: Delete an agent session
	// description: Deletes a session, its transcript, the runs that answered in it and the calls a reader answered.
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// responses:
	//   "204":
	//     description: session deleted
	//   "404":
	//     "$ref": "#/responses/notFound"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	if err := agent_service.DeleteConversation(ctx, session); err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

// SetSessionAutopilot lets a session decide for itself, or stops it doing so.
func SetSessionAutopilot(ctx *context.APIContext) {
	// swagger:operation POST /agent/sessions/{id}/autopilot agent agentSetSessionAutopilot
	// ---
	// summary: Set whether an agent session decides for itself
	// description: Turns autopilot on or off for one session — with it on, a call that would wait for a reader is reviewed by the model instead.
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/AgentAutopilotOption"
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentSession"
	//   "404":
	//     "$ref": "#/responses/notFound"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	form := web.GetForm[*api.AgentAutopilotOption](ctx)
	if err := agent_service.SetConversationAutopilot(ctx, session, form.Autopilot); err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.JSON(http.StatusOK, toSessionResponse(ctx, session, repositoryOfSession(ctx, session)))
}

// ListSessionMessages reads a page of the transcript, oldest first.
func ListSessionMessages(ctx *context.APIContext) {
	// swagger:operation GET /agent/sessions/{id}/messages agent agentListSessionMessages
	// ---
	// summary: List the messages of an agent session
	// description: Reads the transcript oldest first. Pass before with the id of the oldest message already held to read the page before it.
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// - name: before
	//   in: query
	//   description: read the page before this message id
	//   type: integer
	// - name: limit
	//   in: query
	//   description: how many messages to read, at most 200
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentMessageList"
	//   "404":
	//     "$ref": "#/responses/notFound"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	limit := ctx.FormInt("limit")
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	before := ctx.FormInt64("before")

	var messages []*agent_model.Message
	var err error
	if before > 0 {
		messages, err = agent_service.ListMessagesBefore(ctx, session.ID, before, limit)
	} else {
		messages, err = agent_service.ListMessages(ctx, session.ID, limit)
	}
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	hasMore := false
	if len(messages) > 0 {
		hasMore, err = agent_service.HasMessagesBefore(ctx, session.ID, messages[0].ID)
		if err != nil {
			ctx.APIErrorInternal(err)
			return
		}
	}
	list := make([]*api.AgentMessage, 0, len(messages))
	for _, message := range messages {
		list = append(list, toMessageResponse(message))
	}
	ctx.JSON(http.StatusOK, api.AgentMessageList{List: list, HasMore: hasMore})
}

// PostSessionMessage sends a message, which starts a run. It answers as soon as the run is queued:
// what the run does is read from the run, the transcript or the stream.
func PostSessionMessage(ctx *context.APIContext) {
	// swagger:operation POST /agent/sessions/{id}/messages agent agentPostSessionMessage
	// ---
	// summary: Send a message to an agent session
	// description: Sends a message and starts a run that answers it. The answer is queued — read the run, the transcript or the events stream to follow it.
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/PostAgentMessageOption"
	// responses:
	//   "202":
	//     "$ref": "#/responses/AgentRun"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "422":
	//     "$ref": "#/responses/validationError"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	form := web.GetForm[*api.PostAgentMessageOption](ctx)
	_, run, err := agent_service.SubmitUserMessage(ctx, session, ctx.Doer.ID, form.Content)
	if err != nil {
		ctx.APIError(http.StatusBadRequest, err.Error())
		return
	}
	// the message is written, and the run that answers it is queued: a client follows that
	ctx.JSON(http.StatusAccepted, toRunResponse(run))
}

// StopSession stops the run a session is answering, if it has one.
func StopSession(ctx *context.APIContext) {
	// swagger:operation POST /agent/sessions/{id}/stop agent agentStopSession
	// ---
	// summary: Stop the run of an agent session
	// description: Cancels the run the session is answering. A run that already finished is left as it is.
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// responses:
	//   "204":
	//     description: the run was stopped
	//   "404":
	//     "$ref": "#/responses/notFound"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	if err := agent_service.CancelRun(session.ID); err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

// ListSessionApprovals lists the calls of a session that wait for a reader.
func ListSessionApprovals(ctx *context.APIContext) {
	// swagger:operation GET /agent/sessions/{id}/approvals agent agentListSessionApprovals
	// ---
	// summary: List the calls an agent session waits for
	// description: Lists the calls that wait for a reader — a call that needs permission, or a question the run can not go on without.
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentApprovalList"
	//   "404":
	//     "$ref": "#/responses/notFound"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	approvals, err := agent_service.PendingApprovals(ctx, session.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	list := make([]*api.AgentApproval, 0, len(approvals))
	for _, approval := range approvals {
		list = append(list, toApprovalResponse(approval))
	}
	ctx.JSON(http.StatusOK, api.AgentApprovalList{List: list, Total: int64(len(list))})
}

// AnswerSessionApproval answers a call that waits: it is allowed, refused, or answered in words.
func AnswerSessionApproval(ctx *context.APIContext) {
	// swagger:operation POST /agent/sessions/{id}/approvals/{approval_id} agent agentAnswerSessionApproval
	// ---
	// summary: Answer a call an agent session waits for
	// description: Allows or refuses a call that needs permission, or answers a question. The run goes on from the answer.
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// - name: approval_id
	//   in: path
	//   description: call id
	//   type: integer
	//   required: true
	// - name: body
	//   in: body
	//   required: true
	//   schema:
	//     "$ref": "#/definitions/AnswerAgentApprovalOption"
	// responses:
	//   "204":
	//     description: the answer was taken
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	approvalID, err := strconv.ParseInt(ctx.PathParam("approval_id"), 10, 64)
	if err != nil || approvalID <= 0 {
		ctx.APIError(http.StatusBadRequest, "the call id is not a number")
		return
	}
	approval, ok := pendingApprovalOf(ctx, session, approvalID)
	if !ok {
		return
	}
	form := web.GetForm[*api.AnswerAgentApprovalOption](ctx)
	if approval.Kind == agent_model.ToolApprovalKindQuestion {
		if strings.TrimSpace(form.Answer) == "" && !strings.EqualFold(form.Decision, "refuse") {
			ctx.APIError(http.StatusBadRequest, "a question is answered with an answer, or refused")
			return
		}
		if err := agent_service.AnswerQuestion(ctx, session.ID, approvalID, ctx.Doer, form.Answer); err != nil {
			ctx.APIError(http.StatusForbidden, err.Error())
			return
		}
		ctx.Status(http.StatusNoContent)
		return
	}
	switch strings.ToLower(strings.TrimSpace(form.Decision)) {
	case "allow", "refuse":
	default:
		ctx.APIError(http.StatusBadRequest, "decision must be allow or refuse")
		return
	}
	allowed := strings.EqualFold(strings.TrimSpace(form.Decision), "allow")
	if err := agent_service.ResolveApproval(ctx, session.ID, approvalID, ctx.Doer, allowed); err != nil {
		ctx.APIError(http.StatusForbidden, err.Error())
		return
	}
	ctx.Status(http.StatusNoContent)
}

// pendingApprovalOf finds one call of a session that still waits for a reader.
func pendingApprovalOf(ctx *context.APIContext, session *agent_model.Conversation, id int64) (*agent_model.ToolApproval, bool) {
	approvals, err := agent_service.PendingApprovals(ctx, session.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return nil, false
	}
	for _, approval := range approvals {
		if approval.ID == id {
			return approval, true
		}
	}
	ctx.APIError(http.StatusNotFound, "the call is not waiting for a reader")
	return nil, false
}

// SessionEvents streams what happens in a session: the run it answers with, and the calls that
// wait for a reader. A client that holds the stream does not have to poll.
func SessionEvents(ctx *context.APIContext) {
	// swagger:operation GET /agent/sessions/{id}/events agent agentSessionEvents
	// ---
	// summary: Stream what an agent session is doing
	// description: Streams server-sent events while the run of a session works — its state, and the calls waiting for a reader. Each event is a JSON object with a type of run or approval.
	// produces:
	// - text/event-stream
	// parameters:
	// - name: id
	//   in: path
	//   description: session id
	//   type: integer
	//   required: true
	// responses:
	//   "200":
	//     description: the stream
	//     schema:
	//       type: string
	//   "404":
	//     "$ref": "#/responses/notFound"
	session, ok := sessionOf(ctx)
	if !ok {
		return
	}
	events, cancel := agent_service.Subscribe(session.ID)
	defer cancel()

	ctx.Resp.Header().Set("Content-Type", "text/event-stream")
	ctx.Resp.Header().Set("Cache-Control", "no-cache")
	ctx.Resp.WriteHeader(http.StatusOK)

	send := func(eventType string, payload any) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(ctx.Resp, "event: %s\ndata: %s\n\n", eventType, body); err != nil {
			return err
		}
		ctx.Resp.Flush()
		return nil
	}
	if run, err := agent_service.LatestRun(ctx, session.ID); err == nil && run != nil {
		if err := send("run", toRunResponse(run)); err != nil {
			return
		}
	}
	for {
		select {
		case <-ctx.Req.Context().Done():
			return
		case event, open := <-events:
			if !open {
				return
			}
			name := event.Name
			if name == "" {
				name = "run"
			}
			if err := send(name, event.Data); err != nil {
				return
			}
		}
	}
}

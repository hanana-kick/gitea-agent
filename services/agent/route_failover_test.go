// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// groupRouteEndpoint stands in for one provider's model: it answers as the model it was asked
// for, so the member that answered is readable from what the run reports.
type groupRouteEndpoint struct {
	url    string
	status int // non-zero: this member refuses, and the router has to move on

	mu    sync.Mutex
	asked []string
}

func newGroupRouteEndpoint(t *testing.T, status int) *groupRouteEndpoint {
	t.Helper()
	endpoint := &groupRouteEndpoint{status: status}
	server := httptest.NewServer(http.HandlerFunc(endpoint.serve))
	t.Cleanup(server.Close)
	endpoint.url = server.URL
	return endpoint
}

func (e *groupRouteEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	body := struct {
		Model string `json:"model"`
	}{}
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil || json.Unmarshal(raw, &body) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	e.asked = append(e.asked, body.Model)
	e.mu.Unlock()

	if e.status != 0 {
		w.WriteHeader(e.status)
		_, _ = w.Write([]byte(`{"error":{"message":"this member is down"}}`))
		return
	}
	writeJSON(w, map[string]any{
		"id": "member", "model": body.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "answered by " + body.Model},
			"finish_reason": "stop",
		}},
	})
}

func (e *groupRouteEndpoint) modelsAsked() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.asked...)
}

// groupRouteMember is one model behind a group, with the endpoint that answers for it.
type groupRouteMember struct {
	provider *agent_model.Provider
	endpoint *groupRouteEndpoint
	model    string
}

// groupRouteFixture is the world a group-routing test needs: providers whose endpoints stand in
// for the models, and a session to drive.
type groupRouteFixture struct {
	ctx   context.Context
	owner *user_model.User
	key   *agent_model.APIKey
}

func newGroupRouteFixture(t *testing.T) *groupRouteFixture {
	t.Helper()
	require.NoError(t, unittest.PrepareTestDatabase())
	ctx := context.Background()
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	key, err := CreateAPIKey(ctx, owner.ID, "group-routing-"+t.Name(), "sk-test")
	require.NoError(t, err)
	// a run answers only where an agent account is named: its tools work as that account
	previousAgent := setting.Agent.Username
	setting.Agent.Username = owner.Name
	t.Cleanup(func() { setting.Agent.Username = previousAgent })
	return &groupRouteFixture{ctx: ctx, owner: owner, key: key}
}

// member adds a provider whose endpoint answers as name+"-model", or refuses with status.
func (f *groupRouteFixture) member(t *testing.T, name string, status int) *groupRouteMember {
	t.Helper()
	endpoint := newGroupRouteEndpoint(t, status)
	provider := &agent_model.Provider{
		UserID: f.owner.ID, Name: name, Provider: "openai", BaseURL: endpoint.url,
		Model: name + "-model", APIKeyID: f.key.ID,
	}
	require.NoError(t, db.Insert(f.ctx, provider))
	return &groupRouteMember{provider: provider, endpoint: endpoint, model: provider.Model}
}

func (f *groupRouteFixture) group(t *testing.T, name, mode string) *agent_model.ModelGroup {
	t.Helper()
	group := &agent_model.ModelGroup{Name: name, Slug: agent_model.GroupSlugFromName(name), Mode: mode}
	require.NoError(t, db.Insert(f.ctx, group))
	return group
}

func (f *groupRouteFixture) modelMember(t *testing.T, group *agent_model.ModelGroup, position int, member *groupRouteMember) {
	t.Helper()
	require.NoError(t, db.Insert(f.ctx, &agent_model.ModelGroupMember{
		GroupID: group.ID, Position: position, ProviderID: member.provider.ID, Model: member.model,
	}))
}

func (f *groupRouteFixture) groupMember(t *testing.T, group, member *agent_model.ModelGroup, position int) {
	t.Helper()
	require.NoError(t, db.Insert(f.ctx, &agent_model.ModelGroupMember{
		GroupID: group.ID, Position: position, MemberGroupID: member.ID,
	}))
}

// session points the repository role at the group and returns the conversation to drive.
func (f *groupRouteFixture) session(t *testing.T, group *agent_model.ModelGroup) *agent_model.Conversation {
	t.Helper()
	require.NoError(t, SetModelRoleRef(f.ctx, ModelRoleForKind(agent_model.ConversationKindRepository), ModelRefForGroup(group.Slug)))
	conversation, err := CreateConversation(f.ctx, agent_model.ConversationKindRepository, f.owner.ID, 1, "group routing")
	require.NoError(t, err)
	t.Cleanup(func() { _ = CancelRun(conversation.ID) })
	engineOnce.Do(func() { require.NoError(t, InitEngine(f.ctx)) })
	return conversation
}

func groupCursor(t *testing.T, group *agent_model.ModelGroup) int {
	t.Helper()
	row, exist, err := db.GetByID[agent_model.ModelGroup](context.Background(), group.ID)
	require.NoError(t, err)
	require.True(t, exist)
	return row.Cursor
}

// A member that refuses does not end the run: the next member answers, and the run names the
// model it came from.
func TestGroupRoutingFallsOverToTheNextMember(t *testing.T) {
	f := newGroupRouteFixture(t)
	primary := f.member(t, "primary", http.StatusInternalServerError)
	backup := f.member(t, "backup", 0)
	group := f.group(t, "Failover Group", agent_model.GroupModeFallback)
	f.modelMember(t, group, 0, primary)
	f.modelMember(t, group, 1, backup)
	conversation := f.session(t, group)

	run := runTurn(t, conversation, f.owner, "answer me")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	assert.Equal(t, backup.model, run.Model)
	assert.Equal(t, []string{primary.model}, primary.endpoint.modelsAsked())
	assert.Equal(t, []string{backup.model}, backup.endpoint.modelsAsked())
}

// A run that no member could answer names every member it tried, in try order.
func TestGroupRoutingReportsEveryMemberThatFailed(t *testing.T) {
	f := newGroupRouteFixture(t)
	first := f.member(t, "down-a", http.StatusInternalServerError)
	second := f.member(t, "down-b", http.StatusInternalServerError)
	group := f.group(t, "All Down Group", agent_model.GroupModeFallback)
	f.modelMember(t, group, 0, first)
	f.modelMember(t, group, 1, second)
	conversation := f.session(t, group)

	run := runTurn(t, conversation, f.owner, "answer me")
	require.Equal(t, agent_model.RunStatusFailed, run.Status)
	assert.Contains(t, run.Error, "down-a/down-a-model")
	assert.Contains(t, run.Error, "down-b/down-b-model")
	assert.Less(t, strings.Index(run.Error, "down-a"), strings.Index(run.Error, "down-b"))
}

// Round robin hands the next call to the following member, and the group remembers where it got to.
func TestGroupRoutingRotatesTheStartingMemberInRoundRobin(t *testing.T) {
	f := newGroupRouteFixture(t)
	first := f.member(t, "rr-a", 0)
	second := f.member(t, "rr-b", 0)
	group := f.group(t, "Round Robin Group", agent_model.GroupModeRoundRobin)
	f.modelMember(t, group, 0, first)
	f.modelMember(t, group, 1, second)
	conversation := f.session(t, group)

	one := runTurn(t, conversation, f.owner, "first question")
	require.Equal(t, agent_model.RunStatusSucceeded, one.Status, one.Error)
	assert.Equal(t, first.model, one.Model)
	assert.Equal(t, 1, groupCursor(t, group))

	two := runTurn(t, conversation, f.owner, "second question")
	require.Equal(t, agent_model.RunStatusSucceeded, two.Status, two.Error)
	assert.Equal(t, second.model, two.Model)
	assert.Equal(t, 0, groupCursor(t, group))
}

// A group may hold another group: the request reaches the model the nested group fronts.
func TestGroupRoutingAnswersThroughANestedGroup(t *testing.T) {
	f := newGroupRouteFixture(t)
	model := f.member(t, "nested", 0)
	inner := f.group(t, "Nested Inner Group", agent_model.GroupModeFallback)
	f.modelMember(t, inner, 0, model)
	outer := f.group(t, "Nested Outer Group", agent_model.GroupModeFallback)
	f.groupMember(t, outer, inner, 0)
	conversation := f.session(t, outer)

	run := runTurn(t, conversation, f.owner, "answer me")
	require.Equal(t, agent_model.RunStatusSucceeded, run.Status, run.Error)
	assert.Equal(t, model.model, run.Model)
}

// A group that lists itself ends in the depth limit rather than in endless recursion.
func TestGroupRoutingRefusesAGroupThatListsItself(t *testing.T) {
	f := newGroupRouteFixture(t)
	group := f.group(t, "Self Listing Group", agent_model.GroupModeFallback)
	f.groupMember(t, group, group, 0)
	conversation := f.session(t, group)

	run := runTurn(t, conversation, f.owner, "answer me")
	require.Equal(t, agent_model.RunStatusFailed, run.Status)
	assert.Contains(t, run.Error, "role nesting is too deep")
}

// A group with nothing behind it cannot answer, and the run says why.
func TestGroupRoutingRefusesAGroupWithNoMembers(t *testing.T) {
	f := newGroupRouteFixture(t)
	group := f.group(t, "Empty Group", agent_model.GroupModeFallback)
	conversation := f.session(t, group)

	run := runTurn(t, conversation, f.owner, "answer me")
	require.Equal(t, agent_model.RunStatusFailed, run.Status)
	assert.Contains(t, run.Error, "role has no members")
}

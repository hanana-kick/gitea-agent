// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package skills reads the procedures an agent may open by name. A skill reaches a session
// by the same rule tools use: a session may open the skills of its own scope and of every
// narrower one, so an instance-wide session also sees what a repository session can read.
package skills

import (
	"context"
	"errors"
	"fmt"
	"strings"

	agent_model "gitea.dev/models/agent"
	"gitea.dev/models/db"
	"gitea.dev/modules/timeutil"
)

// ErrSkillInvalid is returned when a skill cannot be stored as given.
type ErrSkillInvalid struct {
	Reason string
}

func (err ErrSkillInvalid) Error() string { return err.Reason }

// NameLimit caps a skill name, so a name stays a word the model can call.
const NameLimit = 64

// BodyLimit caps a skill body, so one skill cannot fill the prompt of the run that opens it.
const BodyLimit = 32 * 1024

// reach ranks how far a scope looks; it mirrors the reach of tool scopes.
func reach(scope string) int {
	switch scope {
	case agent_model.SkillScopeServer:
		return 2
	case agent_model.SkillScopeRepository:
		return 1
	}
	return 0
}

// reachOf ranks what a session kind may reach: the resident and a server session look after
// the whole instance, a repository session one repository.
func reachOf(sessionKind string) int {
	switch sessionKind {
	case agent_model.ConversationKindResident, agent_model.ConversationKindServer:
		return 2
	case agent_model.ConversationKindRepository:
		return 1
	}
	return 0
}

// AllowedFor reports whether a session kind may open a skill of this scope.
func AllowedFor(scope, sessionKind string) bool {
	return reach(scope) <= reachOf(sessionKind)
}

// List returns the enabled skills a session kind may open, sorted by name.
func List(ctx context.Context, sessionKind string) ([]*agent_model.Skill, error) {
	all, err := All(ctx)
	if err != nil {
		return nil, err
	}
	usable := make([]*agent_model.Skill, 0, len(all))
	for _, skill := range all {
		if skill.Enabled && AllowedFor(skill.Scope, sessionKind) {
			usable = append(usable, skill)
		}
	}
	return usable, nil
}

// Get returns one skill by name when this session kind may open it.
func Get(ctx context.Context, sessionKind, name string) (*agent_model.Skill, error) {
	skill := &agent_model.Skill{}
	has, err := db.GetEngine(ctx).Where("name = ?", strings.TrimSpace(name)).Get(skill)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrSkillNotFound{Name: name}
	}
	if !skill.Enabled || !AllowedFor(skill.Scope, sessionKind) {
		return nil, ErrSkillNotFound{Name: name}
	}
	return skill, nil
}

// ErrSkillNotFound is returned when a skill does not exist or is out of this session's reach.
type ErrSkillNotFound struct {
	Name string
}

func (err ErrSkillNotFound) Error() string {
	return fmt.Sprintf("no skill named %q is available here", err.Name)
}

// All returns every skill, sorted by name, for the operator page.
func All(ctx context.Context) ([]*agent_model.Skill, error) {
	list := make([]*agent_model.Skill, 0, 8)
	if err := db.GetEngine(ctx).OrderBy("name ASC").Find(&list); err != nil {
		return nil, err
	}
	return list, nil
}

// Save stores a skill, creating it when the name is new and updating it otherwise.
func Save(ctx context.Context, name, description, body, scope string, enabled bool) error {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	body = strings.TrimSpace(body)
	switch {
	case name == "":
		return ErrSkillInvalid{Reason: "the skill needs a name"}
	case len([]rune(name)) > NameLimit:
		return ErrSkillInvalid{Reason: fmt.Sprintf("the name is longer than %d characters", NameLimit)}
	case strings.ContainsAny(name, " \t\n"):
		return ErrSkillInvalid{Reason: "the name is one word, so the model can call it"}
	case description == "":
		return ErrSkillInvalid{Reason: "the skill needs a one line description"}
	case body == "":
		return ErrSkillInvalid{Reason: "the skill needs a body"}
	case len(body) > BodyLimit:
		return ErrSkillInvalid{Reason: fmt.Sprintf("the body is larger than %d KB", BodyLimit/1024)}
	}
	switch scope {
	case agent_model.SkillScopeRepository, agent_model.SkillScopeServer:
	default:
		scope = agent_model.SkillScopeCommon
	}

	skill := &agent_model.Skill{}
	has, err := db.GetEngine(ctx).Where("name = ?", name).Get(skill)
	if err != nil {
		return err
	}
	if has {
		_, err = db.GetEngine(ctx).ID(skill.ID).Cols("description", "body", "scope", "enabled", "updated_unix").
			Update(&agent_model.Skill{
				Description: description,
				Body:        body,
				Scope:       scope,
				Enabled:     enabled,
				UpdatedUnix: timeutil.TimeStampNow(),
			})
		return err
	}
	return db.Insert(ctx, &agent_model.Skill{
		Name:        name,
		Description: description,
		Body:        body,
		Scope:       scope,
		Enabled:     enabled,
	})
}

// Delete removes a skill by name.
func Delete(ctx context.Context, name string) error {
	_, err := db.GetEngine(ctx).Where("name = ?", strings.TrimSpace(name)).Delete(&agent_model.Skill{})
	return err
}

// AsNotFound reports whether err means the session may not open that skill.
func AsNotFound(err error) bool {
	return errors.As(err, &ErrSkillNotFound{})
}

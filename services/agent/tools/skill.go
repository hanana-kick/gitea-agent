// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package tools

import (
	"context"

	user_model "gitea.dev/models/user"
	"gitea.dev/modules/json"
	"gitea.dev/services/agent/skills"
)

// SkillToolName is the tool that opens a skill. The engine needs the name to tell whether
// the catalog it puts in the prompt can be opened at all.
const SkillToolName = "gitea_read_skill"

// readSkillTool opens one of the skills the session was offered. Only a skill's name and
// description reach the prompt, so its steps arrive when the model asks for them.
type readSkillTool struct{}

func (readSkillTool) Name() string { return SkillToolName }

func (readSkillTool) Description() string {
	return "Open one of your skills by name and follow its steps. Call this when a skill " +
		"listed for this session covers what you are asked to do."
}

// every session may open the skills it is offered; the lookup keeps it inside its reach
func (readSkillTool) Scope() Scope { return ScopeUser }

func (readSkillTool) Args() []Arg {
	return []Arg{{Name: "name", Description: "The skill name exactly as it is listed"}}
}

// Offered keeps the tool out of a session that has no skills, so an instance without
// skills pays nothing for it.
func (readSkillTool) Offered(ctx context.Context, session Session) (bool, error) {
	list, err := skills.List(ctx, session.Kind)
	if err != nil {
		return false, err
	}
	return len(list) > 0, nil
}

func (readSkillTool) Run(ctx context.Context, _ *user_model.User, session Session, args string) (string, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return "", err
	}
	skill, err := skills.Get(ctx, session.Kind, params.Name)
	if err != nil {
		if skills.AsNotFound(err) {
			return "No skill by that name is available in this session. Use a name from the skills listed for this session.", nil
		}
		return "", err
	}
	return "Skill " + skill.Name + ":\n\n" + clip(skill.Body), nil
}

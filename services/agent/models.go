// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"strings"

	agent_model "gitea.dev/models/agent"
)

// ParseModelLines parses manually entered models, one per line:
//
//	gpt-4o                          -> id only, the name defaults to the id
//	gpt-4o = GPT-4o                 -> explicit name
//	gpt-4o - GPT-4o                 -> explicit name
//
// Blank lines are ignored. The identifiers themselves may contain "-", so a bare
// dash only separates when it stands alone between two values.
func ParseModelLines(lines string) []agent_model.ProviderModel {
	models := make([]agent_model.ProviderModel, 0)
	for _, raw := range strings.Split(lines, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		id, name, found := strings.Cut(line, "=")
		if !found {
			id, name, found = strings.Cut(line, " - ")
		}
		if !found {
			models = append(models, agent_model.ProviderModel{ID: line})
			continue
		}
		models = append(models, agent_model.ProviderModel{ID: id, Name: name})
	}
	return agent_model.NormalizeModels(models)
}

// FormatModelLines renders models as manually editable lines.
func FormatModelLines(models []agent_model.ProviderModel) string {
	lines := make([]string, 0, len(models))
	for _, m := range models {
		if m.Name == "" || m.Name == m.ID {
			lines = append(lines, m.ID)
			continue
		}
		lines = append(lines, m.ID+" = "+m.Name)
	}
	return strings.Join(lines, "\n")
}

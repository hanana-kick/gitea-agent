// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import "gitea.dev/modules/log"

// Agent is how the instance's agent is known on the surfaces it works on.
var Agent struct {
	// Username is the account the agent writes as on issues and pull requests. When it is
	// empty the agent answers nothing on its own: it is only reachable through its chat.
	Username string
	// MentionsPerMinute bounds how often one repository may reach the agent by naming it, so a
	// busy thread cannot spend the instance's model budget. Zero uses the default.
	MentionsPerMinute int
}

// DefaultMentionsPerMinute is how often a repository may reach the agent when the instance does
// not say otherwise.
const DefaultMentionsPerMinute = 6

func loadAgentFrom(cfg ConfigProvider) {
	section := cfg.Section("agent")
	Agent.Username = section.Key("USERNAME").String()
	Agent.MentionsPerMinute = section.Key("MENTIONS_PER_MINUTE").MustInt(DefaultMentionsPerMinute)
	if Agent.MentionsPerMinute <= 0 {
		Agent.MentionsPerMinute = DefaultMentionsPerMinute
	}
	if Agent.Username != "" {
		log.Info("Agent: comments and reviews are written as %q", Agent.Username)
	}
}

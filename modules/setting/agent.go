// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import "gitea.dev/modules/log"

// Agent is how the instance's agent is known on the surfaces it works on.
var Agent struct {
	// Username is the account the agent writes as on issues and pull requests. When it is
	// empty the agent answers nothing on its own: it is only reachable through its chat.
	Username string
}

func loadAgentFrom(cfg ConfigProvider) {
	section := cfg.Section("agent")
	Agent.Username = section.Key("USERNAME").String()
	if Agent.Username != "" {
		log.Info("Agent: comments and reviews are written as %q", Agent.Username)
	}
}

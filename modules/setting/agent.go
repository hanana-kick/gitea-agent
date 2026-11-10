// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"path/filepath"
	"strings"

	"gitea.dev/modules/log"
)

// Agent is how the instance's agent is known on the surfaces it works on.
var Agent struct {
	// Username is the account the agent writes as on issues and pull requests. When it is
	// empty the agent answers nothing on its own: it is only reachable through its chat.
	Username string
	// MentionsPerMinute bounds how often one repository may reach the agent by naming it, so a
	// busy thread cannot spend the instance's model budget. Zero uses the default.
	MentionsPerMinute int
	// PromptTrace stores what each round of a run handed its model. It is off by default: the
	// traces are what a live run is diagnosed from, and they cost a file per round.
	PromptTrace bool
	// MaxConcurrentSessions is how many sessions the agent answers at once. A request that
	// arrives while that many are working waits in the queue, and says so. Zero uses the default.
	MaxConcurrentSessions int
	// PriorityKinds names the session kinds that go ahead of the queue, so the instance keeps
	// looking after itself while readers wait. Empty uses the default.
	PriorityKinds []string
	// Autopilot lets a run decide for itself instead of asking a reader: a call it would put to
	// someone is reviewed by the model and goes on, and a question is answered with what the run
	// already knows. A session can turn it on for itself even where the instance leaves it off.
	Autopilot bool
}

// DefaultMentionsPerMinute is how often a repository may reach the agent when the instance does
// not say otherwise.
const DefaultMentionsPerMinute = 6

// DefaultMaxConcurrentSessions is how many sessions the agent answers at once when the instance
// does not say otherwise.
const DefaultMaxConcurrentSessions = 4

// DefaultPriorityKinds is which session kinds go ahead of the queue when the instance does not
// say otherwise: the always-on session, which an instance under load must not starve.
var DefaultPriorityKinds = []string{"resident"}

func loadAgentFrom(cfg ConfigProvider) {
	section := cfg.Section("agent")
	Agent.Username = section.Key("USERNAME").String()
	Agent.MentionsPerMinute = section.Key("MENTIONS_PER_MINUTE").MustInt(DefaultMentionsPerMinute)
	if Agent.MentionsPerMinute <= 0 {
		Agent.MentionsPerMinute = DefaultMentionsPerMinute
	}
	Agent.MaxConcurrentSessions = section.Key("MAX_CONCURRENT_SESSIONS").MustInt(DefaultMaxConcurrentSessions)
	if Agent.MaxConcurrentSessions <= 0 {
		Agent.MaxConcurrentSessions = DefaultMaxConcurrentSessions
	}
	if Agent.MaxConcurrentSessions > 1 {
		log.Info("Agent: %d sessions are answered at once, the rest wait in the queue", Agent.MaxConcurrentSessions)
	}
	Agent.PriorityKinds = make([]string, 0, 2)
	for kind := range strings.SplitSeq(section.Key("PRIORITY_KINDS").MustString(strings.Join(DefaultPriorityKinds, ",")), ",") {
		if kind = strings.TrimSpace(kind); kind != "" {
			Agent.PriorityKinds = append(Agent.PriorityKinds, kind)
		}
	}
	Agent.Autopilot = section.Key("AUTOPILOT").MustBool(false)
	if Agent.Autopilot {
		log.Info("Agent: autopilot is on, so runs decide for themselves instead of asking")
	}
	Agent.PromptTrace = section.Key("PROMPT_TRACE").MustBool(false)
	if Agent.PromptTrace {
		log.Info("Agent: what each run is handed is traced under %s", filepath.Join(AppDataPath, "agent-prompt-traces"))
	}
	if Agent.Username != "" {
		log.Info("Agent: comments and reviews are written as %q", Agent.Username)
	}
}

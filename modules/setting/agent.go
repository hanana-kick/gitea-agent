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
	// Instructions names the files a repository leaves for the agent to read before it works in
	// it, in the order they are read. AGENTS.md is the name agents agree on; an instance can name
	// its own instead, and an empty list turns the reading off.
	Instructions []string

	// Commands lets the agent run a command of a repository in a container of this instance's
	// own. It is off, and stays off without an image, because a command is code someone else
	// wrote, running with whatever the instance can reach.
	Commands bool
	// CommandImage is the container a command runs in, e.g. "docker.io/library/golang:1.27".
	// Nothing is run without one: which toolchain a repository needs is the operator's call.
	CommandImage string
	// CommandRuntime is the container runtime to use, docker or podman. Empty looks for one.
	CommandRuntime string
	// CommandTimeout is how long one command may take, in seconds.
	CommandTimeout int
	// CommandMemoryMB and CommandCPUs bound one command: a repository that compiles, or that
	// does not stop, must not take the instance's machine with it.
	CommandMemoryMB int
	CommandCPUs     string
	// CommandNetwork lets a command reach the network. Off by default: a command that fetches
	// what it needs is a command that already had it.
	CommandNetwork bool
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

// DefaultInstructions is the file a repository leaves for the agent when the instance does not
// name one: the name agents of several kinds have come to agree on.
var DefaultInstructions = []string{"AGENTS.md"}

// DefaultCommand* bound one command when the instance does not say otherwise.
const (
	DefaultCommandTimeout  = 120
	DefaultCommandMemoryMB = 2048
	DefaultCommandCPUs     = "2"
)

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
	Agent.Instructions = make([]string, 0, len(DefaultInstructions))
	for name := range strings.SplitSeq(section.Key("INSTRUCTIONS").MustString(strings.Join(DefaultInstructions, ",")), ",") {
		if name = strings.TrimSpace(name); name != "" {
			Agent.Instructions = append(Agent.Instructions, name)
		}
	}
	Agent.Commands = section.Key("COMMANDS").MustBool(false)
	Agent.CommandImage = strings.TrimSpace(section.Key("COMMAND_IMAGE").String())
	Agent.CommandRuntime = strings.TrimSpace(section.Key("COMMAND_RUNTIME").String())
	Agent.CommandTimeout = section.Key("COMMAND_TIMEOUT").MustInt(DefaultCommandTimeout)
	if Agent.CommandTimeout <= 0 {
		Agent.CommandTimeout = DefaultCommandTimeout
	}
	Agent.CommandMemoryMB = section.Key("COMMAND_MEMORY_MB").MustInt(DefaultCommandMemoryMB)
	if Agent.CommandMemoryMB <= 0 {
		Agent.CommandMemoryMB = DefaultCommandMemoryMB
	}
	Agent.CommandCPUs = section.Key("COMMAND_CPUS").MustString(DefaultCommandCPUs)
	Agent.CommandNetwork = section.Key("COMMAND_NETWORK").MustBool(false)
	if Agent.Commands && Agent.CommandImage == "" {
		log.Warn("Agent: COMMANDS is on but no COMMAND_IMAGE is set, so no command is run")
	}
	if Agent.Commands && Agent.CommandImage != "" {
		log.Info("Agent: a session may run a command in %s, for at most %ds", Agent.CommandImage, Agent.CommandTimeout)
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

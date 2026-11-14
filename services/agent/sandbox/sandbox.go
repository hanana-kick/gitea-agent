// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package sandbox runs a command of a repository in a container of the instance's own: a command
// is code someone else wrote, so it runs with the tools it needs and nothing else — no network
// unless the operator says so, bounded memory, bounded processors, bounded time, and the files of
// one checkout mounted at a time.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"gitea.dev/modules/process"
	"gitea.dev/modules/setting"
)

// ErrUnavailable is returned when no command can be run: the feature is off, no image is named, or
// no container runtime was found.
var ErrUnavailable = errors.New("running a command is not available on this instance")

// outputLimit is how much of what a command wrote reaches the model.
const outputLimit = 8 * 1024

// Available reports whether a command can be run at all, and names the runtime that would run it.
func Available() (string, bool) {
	if !setting.Agent.Commands || strings.TrimSpace(setting.Agent.CommandImage) == "" {
		return "", false
	}
	if runtime := strings.TrimSpace(setting.Agent.CommandRuntime); runtime != "" {
		if _, err := exec.LookPath(runtime); err != nil {
			return "", false
		}
		return runtime, true
	}
	for _, candidate := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(candidate); err != nil {
			continue
		}
		return candidate, true
	}
	return "", false
}

// Describe says what a command would run in, for a reader who is told why it cannot.
func Describe() string {
	if _, ok := Available(); ok {
		return setting.Agent.CommandImage
	}
	if !setting.Agent.Commands {
		return "off"
	}
	if strings.TrimSpace(setting.Agent.CommandImage) == "" {
		return "no image"
	}
	return "no runtime"
}

// Run runs one command in the container, with the directory mounted at /workspace, and returns
// what it wrote. The container is taken away whatever happens, and a command that outlives its
// deadline is killed rather than left running.
func Run(ctx context.Context, runtime, dir, command string) (string, error) {
	name := fmt.Sprintf("gitea-agent-%d", time.Now().UnixNano())
	timeout := time.Duration(setting.Agent.CommandTimeout) * time.Second
	args := commandLine(name, dir, command)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// the wrapper terminates the process group, and the container is named so that a command
	// which outlived its deadline is stopped rather than left running
	cmd := process.CommandContext(runCtx, runtime, args...)
	terminate := cmd.Cancel
	cmd.Cancel = func() error {
		_ = exec.Command(runtime, "kill", name).Run()
		return terminate()
	}
	cmd.WaitDelay = 10 * time.Second

	sink := &limitedWriter{buffer: new(bytes.Buffer), limit: outputLimit}
	cmd.Stdout = sink
	cmd.Stderr = sink
	err := cmd.Run()

	written := sink.Text()
	if runCtx.Err() == context.DeadlineExceeded {
		return written, fmt.Errorf("the command did not finish in %ds", setting.Agent.CommandTimeout)
	}
	if err != nil {
		return written, fmt.Errorf("the command left with %w", err)
	}
	return written, nil
}

// commandLine is what a command is run with: a container of its own, taken away when it is done,
// bounded in what it may reach and in what it may spend, with one checkout mounted read-write at
// /workspace and nothing else of the instance visible.
func commandLine(name, dir, command string) []string {
	network := "none"
	if setting.Agent.CommandNetwork {
		network = "bridge"
	}
	return []string{
		"run", "--rm", "--name", name,
		"--network", network,
		"--memory", fmt.Sprintf("%dm", setting.Agent.CommandMemoryMB),
		"--cpus", setting.Agent.CommandCPUs,
		"--pids-limit", "256",
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--tmpfs", "/tmp:rw,size=256m",
		"-e", "HOME=/tmp",
		"-v", dir + ":/workspace",
		"-w", "/workspace",
		setting.Agent.CommandImage,
		"sh", "-lc", command,
	}
}

// limitedWriter keeps the beginning of what a command wrote: enough for a model to act on, and
// not a build log of its own.
type limitedWriter struct {
	mu     sync.Mutex
	buffer *bytes.Buffer
	limit  int
	over   bool
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// what is dropped is still taken, or the command that wrote it reads as a short write
	taken := len(data)
	room := w.limit - w.buffer.Len()
	switch {
	case room <= 0:
		w.over = true
	case len(data) > room:
		w.over = true
		data = data[:room]
		fallthrough
	default:
		if _, err := w.buffer.Write(data); err != nil {
			return 0, err
		}
	}
	return taken, nil
}

// Text is what the model reads: what the command wrote, and that there is more of it when there is.
func (w *limitedWriter) Text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	text := strings.TrimRight(w.buffer.String(), "\n")
	if w.over {
		text += "\n… (the command wrote more than this run reads)"
	}
	return text
}

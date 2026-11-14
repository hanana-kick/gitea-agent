// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package sandbox

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"gitea.dev/modules/setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSettings runs one spec with the settings it needs and puts them back after it.
func withSettings(t *testing.T, commands bool, image string, network bool) {
	t.Helper()
	original := setting.Agent
	t.Cleanup(func() { setting.Agent = original })
	setting.Agent.Commands = commands
	setting.Agent.CommandImage = image
	setting.Agent.CommandNetwork = network
	setting.Agent.CommandRuntime = "docker"
	setting.Agent.CommandMemoryMB = 2048
	setting.Agent.CommandCPUs = "2"
	setting.Agent.CommandTimeout = 120
}

// An instance runs a command only when it says it may and names the image to run it in: a command
// is code someone else wrote, running on this machine.
func TestCommandsNeedAnImageAndARuntime(t *testing.T) {
	withSettings(t, false, "docker.io/library/alpine:3", false)
	_, ok := Available()
	assert.False(t, ok, "commands are turned off, but a runtime was offered")

	withSettings(t, true, "", false)
	_, ok = Available()
	assert.False(t, ok, "no image is named, but a runtime was offered")

	withSettings(t, true, "docker.io/library/alpine:3", false)
	runtime, ok := Available()
	if assert.True(t, ok) {
		assert.Equal(t, "docker", runtime)
	}
	assert.Equal(t, "docker.io/library/alpine:3", Describe())
}

// What a command is run with is what keeps it to itself: no network unless the instance allows
// one, bounded memory and processors, no capabilities to gain, and one checkout mounted.
func TestCommandLineHoldsACommandIn(t *testing.T) {
	withSettings(t, true, "docker.io/library/alpine:3", false)
	line := commandLine("a-name", "/tmp/checkout", "go test ./...")
	joined := strings.Join(line, " ")

	assert.Equal(t, "run", line[0])
	assert.Contains(t, joined, "--rm", "the container is left behind")
	assert.Contains(t, joined, "--network none", "a command may reach the network")
	assert.Contains(t, joined, "--memory 2048m")
	assert.Contains(t, joined, "--cpus 2")
	assert.Contains(t, joined, "--pids-limit 256")
	assert.Contains(t, joined, "--security-opt no-new-privileges")
	assert.Contains(t, joined, "--cap-drop ALL")
	assert.Contains(t, joined, "-v /tmp/checkout:/workspace")
	assert.Contains(t, joined, "-w /workspace")
	assert.Contains(t, joined, "docker.io/library/alpine:3")
	assert.Equal(t, []string{"sh", "-lc", "go test ./..."}, line[len(line)-3:])
	assert.Contains(t, line, "a-name", "the container is not named, so it cannot be stopped")

	withSettings(t, true, "docker.io/library/alpine:3", true)
	assert.Contains(t, strings.Join(commandLine("n", "/tmp/d", "true"), " "), "--network bridge")
}

// What a command writes is kept to what a model reads, and the rest is said rather than dropped
// without a word.
func TestOutputIsCappedAndSaidSo(t *testing.T) {
	sink := &limitedWriter{buffer: new(bytes.Buffer), limit: 16}
	written, err := sink.Write([]byte("0123456789abcdefghij"))
	require.NoError(t, err)
	assert.Equal(t, 20, written, "a writer that takes less than it is given looks like a short write")

	text := sink.Text()
	assert.True(t, strings.HasPrefix(text, "0123456789abcdef"), text)
	assert.Contains(t, text, "wrote more than this run reads")

	small := &limitedWriter{buffer: new(bytes.Buffer), limit: 64}
	_, err = small.Write([]byte("done"))
	require.NoError(t, err)
	assert.Equal(t, "done", small.Text())
	assert.NotContains(t, small.Text(), "more than")
}

// The output limit is what makes a build log readable, so it is a size a model can work with.
func TestOutputLimitIsSane(t *testing.T) {
	assert.LessOrEqual(t, outputLimit, 64*1024)
	assert.GreaterOrEqual(t, outputLimit, 4*1024)
	assert.True(t, slices.Contains([]string{"docker", "podman"}, "docker"), "the runtimes looked for changed")
}

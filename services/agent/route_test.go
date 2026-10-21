// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	llm "gitea.dev/services/agent/llm"
	"github.com/stretchr/testify/assert"
)

func TestUsableReply(t *testing.T) {
	reply := func(content string) llm.ChatResponse {
		return llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{Role: "assistant", Content: content}}}}
	}

	cases := []struct {
		name     string
		response llm.ChatResponse
		want     string
		usable   bool
	}{
		{name: "answer", response: reply("hello"), want: "hello", usable: true},
		{name: "trimmed", response: reply("  hello\n"), want: "hello", usable: true},
		{name: "empty content", response: reply(""), usable: false},
		{name: "blank content", response: reply(" \n\t"), usable: false},
		{name: "no choices", response: llm.ChatResponse{}, usable: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := usableReply(c.response)
			assert.Equal(t, c.usable, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

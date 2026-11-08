// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shape a model writes when the gateway does not carry the call it made. Read as prose, the
// call would never run and the reader would be shown a syntax they did not ask to see.
func TestTextToolCallsReadsCallsWrittenAsText(t *testing.T) {
	content := "The page needs a branch.\n" +
		"<tool_call>\n<function=gitea_create_branch>\n" +
		"<parameter=branch>\npages\n</parameter>\n" +
		"<parameter=from_branch>\nmain\n</parameter>\n" +
		"<parameter=repository>\nadmin/hello-page\n</parameter>\n" +
		"</function>\n</tool_call>\nThe branch is there when this is done."

	calls, rest := TextToolCalls(content)
	require.Len(t, calls, 1)
	assert.Equal(t, "gitea_create_branch", calls[0].Function.Name)
	assert.JSONEq(t, `{"branch":"pages","from_branch":"main","repository":"admin/hello-page"}`, calls[0].Function.Arguments)
	assert.Equal(t, "function", calls[0].Type)
	assert.Equal(t, "The page needs a branch.\nThe branch is there when this is done.", rest)
}

func TestTextToolCallsReadsTheCallsWrittenAsJSON(t *testing.T) {
	content := `<tool_call>{"name": "gitea_read_file", "arguments": {"path": "README.md", "line": 12, "whole": true}}</tool_call>`

	calls, rest := TextToolCalls(content)
	require.Len(t, calls, 1)
	assert.Equal(t, "gitea_read_file", calls[0].Function.Name)
	assert.JSONEq(t, `{"path":"README.md","line":12,"whole":true}`, calls[0].Function.Arguments)
	assert.Empty(t, rest)

	// a group of calls is a group wherever it is written
	grouped := "<tool_calls>\n<tool_call>{\"name\": \"gitea_list_branches\", \"arguments\": {}}</tool_call>\n" +
		"<tool_call>{\"name\": \"gitea_list_commits\", \"arguments\": {}}</tool_call>\n</tool_calls>\ndone"
	calls, rest = TextToolCalls(grouped)
	require.Len(t, calls, 2)
	assert.Equal(t, "gitea_list_branches", calls[0].Function.Name)
	assert.Equal(t, "gitea_list_commits", calls[1].Function.Name)
	assert.Equal(t, "done", rest)
}

// An answer about the format is not a call: what a model writes as code is left as it wrote it.
func TestTextToolCallsLeavesFencedCodeAlone(t *testing.T) {
	content := "A call looks like this:\n\n```\n<tool_call>\n<function=gitea_create_branch>\n<parameter=branch>\npages\n</parameter>\n</function>\n</tool_call>\n```\n"

	calls, rest := TextToolCalls(content)
	assert.Empty(t, calls)
	assert.Equal(t, content, rest)
}

func TestTextToolCallsLeavesAnAnswerWithoutCallsAlone(t *testing.T) {
	for _, content := range []string{"", "The repository publishes nothing yet.", "<function=x>unclosed"} {
		calls, rest := TextToolCalls(content)
		assert.Empty(t, calls, content)
		assert.Equal(t, content, rest, content)
	}

	// a call the model broke off is still syntax of the call format, not words for the reader
	calls, rest := TextToolCalls("I will look into it.\n<tool_call>gitea_create_branch(pages)</tool_call>")
	assert.Empty(t, calls)
	assert.Equal(t, "I will look into it.", rest)
}

package llm

import (
	"testing"

	"gitea.dev/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolCallKeepsWhatAGatewaySentWithIt(t *testing.T) {
	message := Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "call_1", Type: "function",
		Function: FunctionCall{Name: "gitea_read_file", Arguments: `{"path":"README.md"}`},
		Extra:    map[string]any{"extra_content": map[string]any{"google": map[string]any{"thought_signature": "sig-1"}}},
	}}}

	encoded, err := json.Marshal(message)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "thought_signature")

	back := Message{}
	require.NoError(t, json.Unmarshal(encoded, &back))
	require.Len(t, back.ToolCalls, 1)
	assert.Equal(t, "call_1", back.ToolCalls[0].ID)
	assert.Equal(t, "gitea_read_file", back.ToolCalls[0].Function.Name)
	assert.JSONEq(t, `{"path":"README.md"}`, back.ToolCalls[0].Function.Arguments)
	assert.Contains(t, back.ToolCalls[0].Extra, "extra_content")

	// the arguments of a call may arrive as the object itself
	object := ToolCall{}
	require.NoError(t, json.Unmarshal([]byte(`{"id":"c","type":"function","function":{"name":"t","arguments":{"path":"README.md"}}}`), &object))
	assert.JSONEq(t, `{"path":"README.md"}`, object.Function.Arguments)
}

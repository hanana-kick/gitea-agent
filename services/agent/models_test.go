// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	agent_model "gitea.dev/models/agent"
	llm "gitea.dev/services/agent/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseModelLines(t *testing.T) {
	assert.Equal(t, []agent_model.ProviderModel{
		{ID: "gpt-4o"},
		{ID: "claude-sonnet-4", Name: "Claude Sonnet 4"},
		{ID: "meta-llama-3.1-70b", Name: "Llama 70B"},
	}, ParseModelLines("gpt-4o\n\nclaude-sonnet-4 = Claude Sonnet 4\nmeta-llama-3.1-70b - Llama 70B\n"))

	// a bare id keeps its dashes, duplicates collapse, blanks drop
	assert.Equal(t, []agent_model.ProviderModel{{ID: "gpt-4o-mini"}},
		ParseModelLines(" gpt-4o-mini \ngpt-4o-mini\n   "))
	assert.Empty(t, ParseModelLines(""))
}

func TestFormatModelLines(t *testing.T) {
	assert.Equal(t, "gpt-4o", FormatModelLines([]agent_model.ProviderModel{{ID: "gpt-4o"}}))
	assert.Equal(t, "gpt-4o = GPT-4o",
		FormatModelLines([]agent_model.ProviderModel{{ID: "gpt-4o", Name: "GPT-4o"}}))
	assert.Equal(t, "a\nb = B", FormatModelLines([]agent_model.ProviderModel{{ID: "a"}, {ID: "b", Name: "B"}}))
}

func TestProviderModelInputsRoundTrip(t *testing.T) {
	models := agent_model.NormalizeModels([]agent_model.ProviderModel{
		{ID: "qwen/qwen3-vl-8b-instruct", Name: "Qwen3 VL", Inputs: []string{"text", "image", "image"}},
		{ID: "text-only"},
	})
	require.Len(t, models, 2)
	assert.Equal(t, []string{"image", "text"}, models[0].Inputs)
	assert.Nil(t, models[1].Inputs)

	p := &agent_model.Provider{}
	require.NoError(t, p.SetModels(models))
	assert.Equal(t, []string{"image", "text"}, p.Models()[0].Inputs)
	assert.Nil(t, p.Models()[1].Inputs)
}

func TestModelInfoInputModalities(t *testing.T) {
	withArch := llm.ModelInfo{ID: "x", Architecture: &llm.ModelArchitecture{
		InputModalities: []string{"text", "video", " "},
	}}
	assert.Equal(t, []string{"text", "video"}, withArch.InputModalities())

	assert.Nil(t, llm.ModelInfo{ID: "x"}.InputModalities())
}

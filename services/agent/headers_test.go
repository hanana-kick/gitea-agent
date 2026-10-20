// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseHeaderLines(t *testing.T) {
	headers, err := ParseHeaderLines("\n# comment\nX-One: 1\nX-Two: value: with colons\n\n")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"X-One": "1", "X-Two": "value: with colons"}, headers)

	headers, err = ParseHeaderLines("")
	require.NoError(t, err)
	assert.Nil(t, headers)
}

func TestParseHeaderLinesRejects(t *testing.T) {
	var manyHeaders strings.Builder
	for i := 0; i <= maxHeaders; i++ {
		fmt.Fprintf(&manyHeaders, "X-H%d: 1\n", i)
	}

	cases := map[string]string{
		"missing colon":         "X-One 1",
		"empty name":            ": 1",
		"space in name":         "X One: 1",
		"host header":           "Host: evil.example.com",
		"content-length header": "Content-Length: 10",
		"too many headers":      manyHeaders.String(),
		"name too long":         strings.Repeat("X", maxHeaderName+1) + ": 1",
		"value too long":        "X-H: " + strings.Repeat("v", maxHeaderValue+1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHeaderLines(input)
			assert.Error(t, err)
		})
	}
}

func TestFormatHeaderLines(t *testing.T) {
	assert.Empty(t, FormatHeaderLines(nil))
	formatted := FormatHeaderLines(map[string]string{"X-Two": "2", "X-One": "1"})
	assert.Equal(t, "X-One: 1\nX-Two: 2", formatted)

	roundTrip, err := ParseHeaderLines(formatted)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"X-One": "1", "X-Two": "2"}, roundTrip)
}

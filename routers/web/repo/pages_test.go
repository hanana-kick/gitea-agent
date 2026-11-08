// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A published page is read from its own directory: the path of a request never climbs out of it,
// whatever a reader asks for.
func TestPagesPathStaysInsideThePage(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", "index.html"},
		{"index.html", "index.html"},
		{"style.css", "style.css"},
		{"assets/app.css", "assets/app.css"},
		{"about/", "about/index.html"},
		{"../../app.ini", "app.ini"}, // the page directory is the root of the request
		{"../secret", "secret"},
		{"assets/../index.html", "index.html"},
		{"..", "index.html"}, // the page directory is the root, so this is the index
		{"/etc/passwd", "etc/passwd"},
	}
	for _, tc := range cases {
		got, ok := pagesPath(tc.raw)
		assert.True(t, ok, "pagesPath(%q)", tc.raw)
		assert.Equal(t, tc.want, got, "pagesPath(%q)", tc.raw)
		assert.NotContains(t, got, "..", "pagesPath(%q) resolved outside the page", tc.raw)
	}
}

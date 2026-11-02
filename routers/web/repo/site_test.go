// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A published page is read from its own directory: the path of a request never climbs out of it.
func TestPagesPathStaysInsideThePage(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"", "public/index.html", true},
		{"index.html", "public/index.html", true},
		{"style.css", "public/style.css", true},
		{"assets/app.css", "public/assets/app.css", true},
		{"about/", "public/about/index.html", true},
		{"../../app.ini", "public/app.ini", true}, // the page directory is the root of the request
		{"../secret", "public/secret", true},
		{"assets/../index.html", "public/index.html", true},
		{"..", "public/index.html", true}, // the page directory is the root, so this is the index
	}
	for _, tc := range cases {
		got, ok := pagesPath(tc.raw)
		assert.Equal(t, tc.ok, ok, "pagesPath(%q)", tc.raw)
		if ok {
			assert.Equal(t, tc.want, got, "pagesPath(%q)", tc.raw)
			assert.NotContains(t, got, "..", "pagesPath(%q) resolved outside the page", tc.raw)
		}
	}
}

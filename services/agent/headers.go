// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"fmt"
	"slices"
	"strings"
)

const (
	maxHeaders     = 32
	maxHeaderName  = 64
	maxHeaderValue = 1024
)

// ParseHeaderLines parses "Name: Value" lines, as typed into the admin form,
// into a header map. Blank lines and lines starting with "#" are ignored.
func ParseHeaderLines(lines string) (map[string]string, error) {
	headers := map[string]string{}
	for lineNo, raw := range strings.Split(lines, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !found || name == "" {
			return nil, fmt.Errorf("line %d: expected \"Name: Value\", got %q", lineNo+1, line)
		}
		if err := validateHeader(name, value); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		headers[name] = value
		if len(headers) > maxHeaders {
			return nil, fmt.Errorf("at most %d headers are allowed", maxHeaders)
		}
	}
	if len(headers) == 0 {
		return nil, nil
	}
	return headers, nil
}

// FormatHeaderLines renders headers as sorted "Name: Value" lines for the admin form.
func FormatHeaderLines(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, name+": "+headers[name])
	}
	return strings.Join(lines, "\n")
}

func validateHeader(name, value string) error {
	if len(name) > maxHeaderName {
		return fmt.Errorf("header name %q is too long", name)
	}
	for _, r := range name {
		if !isTokenRune(r) {
			return fmt.Errorf("header name %q contains invalid characters", name)
		}
	}
	if len(value) > maxHeaderValue {
		return fmt.Errorf("value of header %q is too long", name)
	}
	// Go's http.Header cannot carry these: Host comes from the URL, Content-Length from the body.
	switch strings.ToLower(name) {
	case "host", "content-length":
		return fmt.Errorf("header %q can not be set", name)
	}
	return nil
}

// isTokenRune reports whether r may appear in an HTTP header field name (RFC 7230 token).
func isTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

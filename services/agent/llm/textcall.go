// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"encoding/json"
	"strings"
)

// The tags models trained to write calls as text put them in. A gateway that does not carry a
// call in the structure the standard asks for leaves one of these in the answer instead.
const (
	textCallOpen   = "<tool_call>"
	textCallClose  = "</tool_call>"
	textCallsOpen  = "<tool_calls>"
	textCallsClose = "</tool_calls>"
	textFunctionAt = "<function="
	textParameter  = "<parameter="
)

// TextToolCalls reads the calls a model wrote into its answer instead of returning them as
// calls, and returns the answer with those calls taken out of it. A call the model wrote is a
// call: read as prose, the work it asks for would never happen and the reader would be shown the
// syntax of a language they did not ask to see. A fenced code block is left alone, because a
// model explaining the format is writing about calls, not making them.
func TextToolCalls(content string) ([]ToolCall, string) {
	if !strings.Contains(content, textCallOpen) && !strings.Contains(content, textCallsOpen) {
		return nil, content
	}
	calls := make([]ToolCall, 0, 2)
	var rest strings.Builder
	for _, segment := range splitOffCodeFences(content) {
		if segment.fenced {
			rest.WriteString(segment.text)
			continue
		}
		calls = append(calls, callsInText(segment.text, &rest)...)
	}
	answer := rest.String()
	if len(calls) == 0 {
		return nil, answer
	}
	return calls, strings.TrimSpace(tidyAnswer(answer))
}

// callsInText reads every call of a piece of text and writes what is left of it to answer.
func callsInText(text string, answer *strings.Builder) []ToolCall {
	calls := make([]ToolCall, 0, 1)
	for {
		start, end := findTag(text, textCallsOpen, textCallsClose)
		group := start >= 0
		if !group {
			start, end = findTag(text, textCallOpen, textCallClose)
		}
		if start < 0 {
			answer.WriteString(text)
			return calls
		}
		inner := text[start:end]
		before := text[:start]
		// a call usually stands on lines of its own, and the lines it stood on are not a gap in
		// what the model wrote
		lineEnded := strings.HasSuffix(before, "\n")
		if lineEnded {
			before = before[:len(before)-1]
		}
		answer.WriteString(before)
		text = text[end:]
		// one of the two lines the call stood between keeps its end, so the words before and
		// after it stay on their own lines
		if !lineEnded && strings.HasPrefix(text, "\n") {
			text = text[1:]
		}
		switch {
		case group:
			// the outer tag of a group holds the calls themselves
			calls = append(calls, callsInText(inner[len(textCallsOpen):len(inner)-len(textCallsClose)], &strings.Builder{})...)
		default:
			payload := inner[len(textCallOpen) : len(inner)-len(textCallClose)]
			if call, ok := readTextCall(payload); ok {
				calls = append(calls, call)
			}
		}
	}
}

// findTag returns the offsets of a tag and its closing tag, both included, or -1 when the tag is
// not closed. Gates open either way for tags written in another case than the convention.
func findTag(text, opening, closing string) (int, int) {
	lower := strings.ToLower(text)
	start := strings.Index(lower, opening)
	if start < 0 {
		return -1, -1
	}
	_, after, found := strings.Cut(lower[start:], closing)
	if !found {
		return -1, -1
	}
	return start, len(text) - len(after)
}

// readTextCall reads one call: the JSON object some models write, or the function and parameter
// tags others do.
func readTextCall(payload string) (ToolCall, bool) {
	payload = strings.TrimSpace(payload)
	if strings.HasPrefix(payload, "{") {
		return readJSONTextCall(payload)
	}
	if !strings.Contains(strings.ToLower(payload), textFunctionAt) {
		return ToolCall{}, false
	}
	name := strings.TrimSpace(payloadBetween(payload, textFunctionAt, ">"))
	if name == "" {
		return ToolCall{}, false
	}
	arguments := make(map[string]any, 2)
	for {
		start := strings.Index(strings.ToLower(payload), textParameter)
		if start < 0 {
			break
		}
		tag := payload[start+len(textParameter):]
		tagEnd := strings.Index(tag, ">")
		if tagEnd < 0 {
			break
		}
		key := strings.TrimSpace(tag[:tagEnd])
		valueEnd := strings.Index(tag, "</parameter>")
		if key == "" || valueEnd < 0 {
			break
		}
		arguments[key] = textValue(strings.TrimSpace(tag[tagEnd+1 : valueEnd]))
		payload = tag[valueEnd+len("</parameter>"):]
	}
	return newTextCall(name, arguments), true
}

// readJSONTextCall reads the call some models write with the arguments as a JSON object.
func readJSONTextCall(payload string) (ToolCall, bool) {
	var body struct {
		Name       string `json:"name"`
		Arguments  any    `json:"arguments"`
		Parameters any    `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil || body.Name == "" {
		return ToolCall{}, false
	}
	arguments := body.Arguments
	if arguments == nil {
		arguments = body.Parameters
	}
	if text, ok := arguments.(string); ok {
		return ToolCall{ID: "", Type: "function", Function: FunctionCall{Name: body.Name, Arguments: text}}, true
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return ToolCall{}, false
	}
	return ToolCall{ID: "", Type: "function", Function: FunctionCall{Name: body.Name, Arguments: string(encoded)}}, true
}

// newTextCall builds a call from the parameters a model wrote as text.
func newTextCall(name string, arguments map[string]any) ToolCall {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		encoded = []byte("{}")
	}
	return ToolCall{ID: "", Type: "function", Function: FunctionCall{Name: name, Arguments: string(encoded)}}
}

// textValue reads a parameter value written as text: a number, a boolean or a structure keeps
// its shape, and everything else is the string it was written as.
func textValue(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if value == "" {
		return ""
	}
	if (value[0] == '{' || value[0] == '[') && json.Valid([]byte(value)) {
		var decoded any
		if json.Unmarshal([]byte(value), &decoded) == nil {
			return decoded
		}
	}
	var number json.Number
	if err := json.Unmarshal([]byte(value), &number); err == nil {
		return number
	}
	return value
}

// payloadBetween returns what stands between a tag and the next ">".
func payloadBetween(text, tag, end string) string {
	lower := strings.ToLower(text)
	start := strings.Index(lower, tag)
	if start < 0 {
		return ""
	}
	rest := text[start+len(tag):]
	name, _, found := strings.Cut(rest, end)
	if !found {
		return ""
	}
	return name
}

// textSegment is a piece of an answer, and whether it is a fenced code block.
type textSegment struct {
	text   string
	fenced bool
}

// splitOffCodeFences cuts an answer into what is written as code and what is not.
func splitOffCodeFences(content string) []textSegment {
	segments := make([]textSegment, 0, 4)
	rest := content
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			segments = append(segments, textSegment{text: rest})
			return segments
		}
		end := strings.Index(rest[start+3:], "```")
		if end < 0 {
			segments = append(segments, textSegment{text: rest})
			return segments
		}
		segments = append(segments, textSegment{text: rest[:start]})
		segments = append(segments, textSegment{text: rest[start : start+3+end+3], fenced: true})
		rest = rest[start+3+end+3:]
	}
}

// tidyAnswer closes the gaps a call left in the answer: what is left is what the model wrote
// besides the calls.
func tidyAnswer(answer string) string {
	var builder strings.Builder
	blank := false
	for line := range strings.SplitSeq(answer, "\n") {
		if strings.TrimSpace(line) == "" {
			if builder.Len() == 0 || blank {
				continue
			}
			blank = true
			builder.WriteString("\n")
			continue
		}
		blank = false
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	return strings.TrimSpace(builder.String())
}

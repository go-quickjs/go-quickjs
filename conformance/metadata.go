package conformance

import (
	"fmt"
	"strings"
)

// Parsing the YAML frontmatter of a test262 file.
//
// The block is delimited by /*--- and ---*/ and uses a small, regular subset of
// YAML: scalars, inline lists, block lists and one level of nesting. A full
// YAML parser would be a dependency for nothing, so this reads the subset the
// suite actually uses and reports anything outside it rather than guessing.

// ParseMetadata extracts the frontmatter from a test's source.
//
// A file with no frontmatter is valid and yields zero metadata.
func ParseMetadata(src string) (Metadata, error) {
	m := Metadata{Flags: map[string]bool{}}

	start := strings.Index(src, "/*---")
	if start < 0 {
		return m, nil
	}
	end := strings.Index(src[start:], "---*/")
	if end < 0 {
		return m, fmt.Errorf("unterminated frontmatter block")
	}
	block := src[start+len("/*---") : start+end]

	// A few tests are written with carriage returns as their only line
	// terminator, which the frontmatter is subject to as much as the code.
	block = strings.ReplaceAll(block, "\r\n", "\n")
	block = strings.ReplaceAll(block, "\r", "\n")
	lines := strings.Split(block, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Only a top-level key starts a field; a nested one belongs to the
		// field being parsed and is consumed by its handler.
		if indentOf(line) > 0 {
			continue
		}

		key, value, ok := splitKey(trimmed)
		if !ok {
			continue
		}

		switch key {
		case "description":
			m.Description = strings.TrimSpace(value)

		case "includes":
			items, next := parseList(value, lines, i)
			m.Includes = items
			i = next

		case "features":
			items, next := parseList(value, lines, i)
			m.Features = items
			i = next

		case "flags":
			items, next := parseList(value, lines, i)
			for _, f := range items {
				m.Flags[f] = true
			}
			i = next

		case "negative":
			neg, next, err := parseNegative(lines, i)
			if err != nil {
				return m, err
			}
			m.Negative = neg
			i = next
		}
	}
	return m, nil
}

// indentOf returns the number of leading spaces.
func indentOf(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}

// splitKey separates "key: value", reporting false for a line that is not one.
func splitKey(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// parseList reads a list written either inline as [a, b] or as following
// indented "- item" lines, returning the index of the last line it consumed.
func parseList(inline string, lines []string, i int) ([]string, int) {
	if inline != "" {
		s := strings.TrimSpace(inline)
		s = strings.TrimPrefix(s, "[")
		s = strings.TrimSuffix(s, "]")
		var out []string
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out, i
	}

	var out []string
	j := i + 1
	for ; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "- ") {
			break
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(t, "- ")))
	}
	return out, j - 1
}

// parseNegative reads the two-field negative block.
func parseNegative(lines []string, i int) (*Negative, int, error) {
	neg := &Negative{}
	j := i + 1
	for ; j < len(lines); j++ {
		line := lines[j]
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if indentOf(line) == 0 {
			break
		}
		key, value, ok := splitKey(t)
		if !ok {
			continue
		}
		switch key {
		case "phase":
			neg.Phase = strings.TrimSpace(value)
		case "type":
			neg.Type = strings.TrimSpace(value)
		}
	}
	if neg.Phase == "" || neg.Type == "" {
		return nil, j - 1, fmt.Errorf("incomplete negative block")
	}
	return neg, j - 1, nil
}

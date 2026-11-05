package matrix

import (
	"fmt"
	"strconv"
	"strings"
)

// This file reads the YAML subset that docs/faz0/tarama/pairs.py writes:
// block mappings and sequences indented with spaces, single-line flow
// sequences and mappings, double-quoted and plain scalars, and comments.
// Anything else (tabs, block scalars, anchors, single quotes, multi-line
// flows) is an error, so the reader cannot silently misread the matrix.
//
// A YAML library is not on the approved dependency list (ADR-0003); this
// reader is the stopgap and is replaced by one if a library is approved.

type line struct {
	no      int
	indent  int
	content string
}

// parseYAML parses a document into map[string]any, []any, string, bool
// and nil values.
func parseYAML(src string) (any, error) {
	var lines []line
	for i, raw := range strings.Split(src, "\n") {
		raw = strings.TrimRight(raw, "\r")
		if strings.ContainsRune(raw, '\t') {
			return nil, fmt.Errorf("line %d: tab characters are not supported", i+1)
		}
		content, err := stripComment(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		content = strings.TrimRight(content, " ")
		if strings.TrimSpace(content) == "" {
			continue
		}
		indent := len(content) - len(strings.TrimLeft(content, " "))
		lines = append(lines, line{no: i + 1, indent: indent, content: content[indent:]})
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	if lines[0].indent != 0 {
		return nil, fmt.Errorf("line %d: the document must start at column 0", lines[0].no)
	}
	p := &blockParser{lines: lines}
	v, err := p.block(0)
	if err != nil {
		return nil, err
	}
	if p.i != len(p.lines) {
		return nil, fmt.Errorf("line %d: unexpected indentation", p.lines[p.i].no)
	}
	return v, nil
}

// stripComment removes a comment: '#' at the start or after a space,
// outside double quotes.
func stripComment(s string) (string, error) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case inQuote && c == '\\':
			i++
		case c == '"':
			inQuote = !inQuote
		case !inQuote && c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i], nil
		case !inQuote && (c == '\'' && (i == 0 || s[i-1] == ' ' || s[i-1] == '[' || s[i-1] == '{')):
			return "", fmt.Errorf("single-quoted scalars are not supported")
		}
	}
	if inQuote {
		return "", fmt.Errorf("unterminated double quote")
	}
	return s, nil
}

type blockParser struct {
	lines []line
	i     int
}

func (p *blockParser) block(indent int) (any, error) {
	l := p.lines[p.i]
	if l.indent != indent {
		return nil, fmt.Errorf("line %d: indentation %d, want %d", l.no, l.indent, indent)
	}
	if isSeqItem(l.content) {
		return p.sequence(indent)
	}
	return p.mapping(indent)
}

func isSeqItem(s string) bool { return s == "-" || strings.HasPrefix(s, "- ") }

func (p *blockParser) sequence(indent int) (any, error) {
	out := []any{}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent < indent {
			break
		}
		if l.indent > indent || !isSeqItem(l.content) {
			return nil, fmt.Errorf("line %d: expected a sequence item at column %d", l.no, indent)
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.content, "-"), " ")
		switch {
		case rest == "":
			p.i++
			if p.i >= len(p.lines) || p.lines[p.i].indent <= indent {
				out = append(out, nil)
				continue
			}
			v, err := p.block(p.lines[p.i].indent)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		case startsMapping(rest):
			// "- key: value": the item is a mapping whose first entry
			// sits on the dash line, the rest indented to match it.
			itemIndent := indent + (len(l.content) - len(rest))
			p.lines[p.i] = line{no: l.no, indent: itemIndent, content: rest}
			v, err := p.mapping(itemIndent)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		default:
			v, err := parseInline(rest)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", l.no, err)
			}
			out = append(out, v)
			p.i++
		}
	}
	return out, nil
}

// startsMapping reports whether a sequence item's text is "key: value" or
// "key:" rather than a scalar or a flow collection.
func startsMapping(s string) bool {
	if s == "" || strings.ContainsRune(`"[{`, rune(s[0])) {
		return false
	}
	return strings.Contains(s, ": ") || strings.HasSuffix(s, ":")
}

func (p *blockParser) mapping(indent int) (any, error) {
	out := map[string]any{}
	for p.i < len(p.lines) {
		l := p.lines[p.i]
		if l.indent < indent {
			break
		}
		if l.indent > indent || isSeqItem(l.content) {
			return nil, fmt.Errorf("line %d: expected a mapping key at column %d", l.no, indent)
		}
		key, value, nested := splitKey(l.content)
		if key == "" {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", l.no)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.no, key)
		}
		p.i++
		if !nested {
			v, err := parseInline(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", l.no, err)
			}
			out[key] = v
			continue
		}
		if p.i >= len(p.lines) || p.lines[p.i].indent <= indent {
			out[key] = nil
			continue
		}
		v, err := p.block(p.lines[p.i].indent)
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}

// splitKey splits "key: value" (nested false) or "key:" (nested true).
// Keys are plain scalars; they may contain ':' not followed by a space
// ("user:u_worker").
func splitKey(s string) (key, value string, nested bool) {
	if k, v, ok := strings.Cut(s, ": "); ok {
		return strings.TrimSpace(k), strings.TrimSpace(v), false
	}
	if strings.HasSuffix(s, ":") {
		return strings.TrimSpace(strings.TrimSuffix(s, ":")), "", true
	}
	return "", "", false
}

// parseInline parses a value on one line: a flow collection, a quoted
// scalar or a plain scalar.
func parseInline(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if s[0] != '[' && s[0] != '{' && s[0] != '"' {
		return plainScalar(s), nil
	}
	f := &flowParser{s: s}
	v, err := f.value()
	if err != nil {
		return nil, err
	}
	f.skipSpace()
	if f.i != len(f.s) {
		return nil, fmt.Errorf("unexpected %q after value", f.s[f.i:])
	}
	return v, nil
}

func plainScalar(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null", "~":
		return nil
	}
	return s
}

type flowParser struct {
	s string
	i int
}

func (f *flowParser) skipSpace() {
	for f.i < len(f.s) && f.s[f.i] == ' ' {
		f.i++
	}
}

func (f *flowParser) value() (any, error) {
	f.skipSpace()
	if f.i >= len(f.s) {
		return nil, fmt.Errorf("missing value")
	}
	switch f.s[f.i] {
	case '[':
		return f.list()
	case '{':
		return f.dict()
	case '"':
		return f.quoted()
	}
	start := f.i
	for f.i < len(f.s) && !strings.ContainsRune(",]}", rune(f.s[f.i])) {
		f.i++
	}
	return plainScalar(strings.TrimSpace(f.s[start:f.i])), nil
}

func (f *flowParser) quoted() (string, error) {
	start := f.i
	f.i++
	for f.i < len(f.s) {
		switch f.s[f.i] {
		case '\\':
			f.i += 2
			continue
		case '"':
			f.i++
			v, err := strconv.Unquote(f.s[start:f.i])
			if err != nil {
				return "", fmt.Errorf("quoted scalar %s: %w", f.s[start:f.i], err)
			}
			return v, nil
		}
		f.i++
	}
	return "", fmt.Errorf("unterminated double quote")
}

func (f *flowParser) list() (any, error) {
	f.i++ // [
	out := []any{}
	f.skipSpace()
	if f.i < len(f.s) && f.s[f.i] == ']' {
		f.i++
		return out, nil
	}
	for {
		v, err := f.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		f.skipSpace()
		if f.i >= len(f.s) {
			return nil, fmt.Errorf("unterminated flow sequence")
		}
		switch f.s[f.i] {
		case ',':
			f.i++
		case ']':
			f.i++
			return out, nil
		default:
			return nil, fmt.Errorf("unexpected %q in flow sequence", f.s[f.i])
		}
	}
}

func (f *flowParser) dict() (any, error) {
	f.i++ // {
	out := map[string]any{}
	f.skipSpace()
	if f.i < len(f.s) && f.s[f.i] == '}' {
		f.i++
		return out, nil
	}
	for {
		f.skipSpace()
		var key string
		if f.i < len(f.s) && f.s[f.i] == '"' {
			k, err := f.quoted()
			if err != nil {
				return nil, err
			}
			key = k
		} else {
			start := f.i
			for f.i < len(f.s) && f.s[f.i] != ':' && !strings.ContainsRune(",{}[]", rune(f.s[f.i])) {
				f.i++
			}
			key = strings.TrimSpace(f.s[start:f.i])
		}
		if key == "" || f.i >= len(f.s) || f.s[f.i] != ':' {
			return nil, fmt.Errorf("expected \"key: value\" in flow mapping")
		}
		f.i++
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate key %q in flow mapping", key)
		}
		v, err := f.value()
		if err != nil {
			return nil, err
		}
		out[key] = v
		f.skipSpace()
		if f.i >= len(f.s) {
			return nil, fmt.Errorf("unterminated flow mapping")
		}
		switch f.s[f.i] {
		case ',':
			f.i++
		case '}':
			f.i++
			return out, nil
		default:
			return nil, fmt.Errorf("unexpected %q in flow mapping", f.s[f.i])
		}
	}
}

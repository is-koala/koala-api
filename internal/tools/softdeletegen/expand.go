package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	directivePrefix = "-- +koala "
	beginMarker     = "-- +koala begin generated (make generate); do not edit"
	endMarker       = "-- +koala end generated"
)

// directive is `-- +koala <kind> <version> <table> [security_barrier]`.
var directive = regexp.MustCompile(`^-- \+koala (soft-delete|soft-delete-view) (v[0-9]+) ([a-z][a-z0-9_]*)( security_barrier)?$`)

// maxTable keeps the longest derived name, <table>_deleted_together,
// within Postgres' 63-byte identifiers (longer names are truncated).
const maxTable = 63 - len("_deleted_together")

// expand rewrites every directive's generated block of one migration file.
func expand(src string) (string, error) {
	lines := strings.Split(src, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case line == beginMarker || line == endMarker:
			return "", fmt.Errorf("line %d: generated block without a directive above it", i+1)
		case !strings.HasPrefix(line, directivePrefix):
			out = append(out, line)
			continue
		}
		block, err := render(line)
		if err != nil {
			return "", fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, line)
		if i+1 < len(lines) && lines[i+1] == beginMarker {
			end := i + 2
			for end < len(lines) && lines[end] != endMarker {
				end++
			}
			if end == len(lines) {
				return "", fmt.Errorf("line %d: %q has no %q", i+2, beginMarker, endMarker)
			}
			i = end
		}
		out = append(out, block...)
	}
	return strings.Join(out, "\n"), nil
}

// render returns the generated block of one directive line, markers
// included.
func render(line string) ([]string, error) {
	m := directive.FindStringSubmatch(line)
	if m == nil {
		return nil, fmt.Errorf("unknown directive %q; want `-- +koala soft-delete|soft-delete-view v1 <table> [security_barrier]`", line)
	}
	kind, version, table, barrier := m[1], m[2], m[3], m[4] != ""
	if version != "v1" {
		return nil, fmt.Errorf("unknown template version %s", version)
	}
	if len(table) > maxTable {
		return nil, fmt.Errorf("table name %s is longer than %d bytes", table, maxTable)
	}
	if strings.HasPrefix(table, "active_") {
		return nil, errors.New("a soft-deleted table cannot be named active_*")
	}
	block := []string{beginMarker}
	if kind == "soft-delete" {
		block = append(block, softDeleteV1(table)...)
	}
	block = append(block, viewV1(table, barrier)...)
	return append(block, endMarker), nil
}

// softDeleteV1 is the frozen v1 template of a table's soft-delete columns.
// Never change it: applied migrations contain its output.
func softDeleteV1(t string) []string {
	return []string{
		"ALTER TABLE " + t,
		"    ADD COLUMN deleted_at timestamptz,",
		"    ADD COLUMN deletion_id uuid REFERENCES deletions (id),",
		"    ADD CONSTRAINT " + t + "_deleted_together CHECK ((deleted_at IS NULL) = (deletion_id IS NULL));",
		"CREATE INDEX " + t + "_deletion_id_idx ON " + t + " (deletion_id) WHERE deletion_id IS NOT NULL;",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON " + t + " TO koala_maint;",
	}
}

// viewV1 is the frozen v1 template of a table's active view and its grant.
// No CHECK OPTION: the soft delete is an UPDATE through the view that moves
// the row out of it. A view filtering personal data is a security barrier
// (ADR-0017 §8).
func viewV1(t string, barrier bool) []string {
	opt := ""
	if barrier {
		opt = " WITH (security_barrier)"
	}
	return []string{
		"CREATE VIEW active_" + t + opt + " AS",
		"    SELECT * FROM " + t + " WHERE deleted_at IS NULL;",
		"GRANT SELECT, INSERT, UPDATE ON active_" + t + " TO koala_app;",
	}
}

// writeRule replaces the rule line of sqlc.yaml's no-base-table rule.
func writeRule(src, rule string) (string, error) {
	lines := strings.Split(src, "\n")
	inRule := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "- name: no-base-table":
			inRule = true
		case inRule && strings.HasPrefix(trimmed, "- name:"):
			return "", errors.New("rule no-base-table has no rule line")
		case inRule && strings.HasPrefix(trimmed, "rule:"):
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			lines[i] = indent + "rule: '" + strings.ReplaceAll(rule, "'", "''") + "'"
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", errors.New("rule no-base-table not found")
}

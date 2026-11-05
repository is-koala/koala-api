package softdelete

import (
	"fmt"
	"regexp"
	"strings"
)

// NoBaseTablePattern returns the RE2 pattern that finds a soft-deleted base
// table named after FROM, JOIN, UPDATE, INTO, USING or TRUNCATE, or "" when
// the catalog has no soft-deleted table.
func NoBaseTablePattern(c *Catalog) string {
	tables := c.SoftDeleteTables()
	if len(tables) == 0 {
		return ""
	}
	return `(?i)\b(?:from|join|update|into|using|truncate)\s+(?:only\s+)?(?:table\s+)?(?:public\.)?(?:` +
		strings.Join(tables, "|") + `)\b`
}

// NoBaseTableRule returns the CEL expression of sqlc.yaml's no-base-table
// rule for c: true for a query that names a soft-deleted base table. CEL's
// matches() is RE2, the syntax of Go's regexp, so the pattern behaves the
// same in tests. With no soft-deleted table the rule is false.
func NoBaseTableRule(c *Catalog) string {
	p := NoBaseTablePattern(c)
	if p == "" {
		return "false"
	}
	return `query.sql.matches(r"` + p + `")`
}

// JoinFinding is an INNER (or RIGHT, or CROSS) join to the active view of a
// parent on a detach edge: the join drops the child rows whose parent is
// deleted, the trap of Java #309/#321 (ADR-0013 Karar 4).
type JoinFinding struct {
	Join string
	Edge Edge
}

// String describes the finding and its fix.
func (f JoinFinding) String() string {
	return fmt.Sprintf("%s on detach edge %s: use LEFT JOIN %s%s", f.Join, f.Edge, ViewPrefix, f.Edge.Parent)
}

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlString       = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlToken        = regexp.MustCompile(`[a-z_][a-z0-9_$]*|[^\sa-z_]`)
)

// CheckJoins is the JOIN lint of ADR-0013: it reports every join to
// active_<parent> that is not a LEFT (or FULL) join in a query that also
// names the child of a detach edge to that parent, as child or
// active_<child>. It reads text, not a syntax tree: a comma join is not seen;
// the generated read tests are the behavioral guard (ADR-0020).
func CheckJoins(c *Catalog, sql string) []JoinFinding {
	text := strings.ToLower(sql)
	text = sqlBlockComment.ReplaceAllString(text, " ")
	text = sqlLineComment.ReplaceAllString(text, " ")
	text = sqlString.ReplaceAllString(text, "''")
	tokens := sqlToken.FindAllString(text, -1)

	named := map[string]bool{}
	for _, tok := range tokens {
		named[strings.TrimPrefix(tok, ViewPrefix)] = true
	}

	var out []JoinFinding
	for i, tok := range tokens {
		if tok != "join" || i+1 >= len(tokens) {
			continue
		}
		j := i + 1
		for j < len(tokens) && (tokens[j] == "only" || tokens[j] == "public" || tokens[j] == ".") {
			j++
		}
		if j >= len(tokens) || !strings.HasPrefix(tokens[j], ViewPrefix) {
			continue
		}
		parent := strings.TrimPrefix(tokens[j], ViewPrefix)
		kind := joinKind(tokens[:i])
		if kind == "left" || kind == "full" {
			continue
		}
		for _, e := range c.edgesFrom(parent) {
			if e.Policy == Detach && named[e.Child] {
				out = append(out, JoinFinding{Join: strings.ToUpper(kind) + " JOIN " + tokens[j], Edge: e})
			}
		}
	}
	return out
}

// joinKind returns the join type of a JOIN keyword from the tokens before
// it: left, right, full, cross, natural or inner.
func joinKind(before []string) string {
	n := len(before)
	if n > 0 && before[n-1] == "outer" {
		n--
	}
	if n == 0 {
		return "inner"
	}
	switch before[n-1] {
	case "left", "right", "full", "cross", "natural":
		return before[n-1]
	default:
		return "inner"
	}
}

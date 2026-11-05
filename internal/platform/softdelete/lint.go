package softdelete

import (
	"fmt"
	"regexp"
	"strings"
)

// Names of sqlc.yaml's vet rules (ADR-0020).
const (
	RuleNoBaseTable         = "no-base-table"
	RuleNoSoftDeleteColumns = "no-soft-delete-columns"
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

// NoSoftDeleteColumnsPatterns returns the RE2 patterns that find a query
// assigning deleted_at or deletion_id: after UPDATE … SET (which also covers
// ON CONFLICT DO UPDATE SET), or in an INSERT column list. Quoted names are
// caught too. A comparison like `deletion_id = $1` after SET is also
// reported; an application query has no reason to filter on the columns,
// its view already does.
func NoSoftDeleteColumnsPatterns() []string {
	// \x22 is the double quote: a raw CEL string cannot contain one.
	const column = `\x22?\b(?:deleted_at|deletion_id)\b\x22?`
	return []string{
		`(?is)\bupdate\b.*\bset\b.*` + column + `\s*=`,
		`(?is)\binsert\s+into\s+[^(]*\([^)]*` + column,
	}
}

// NoSoftDeleteColumnsRule returns the CEL expression of sqlc.yaml's
// no-soft-delete-columns rule.
func NoSoftDeleteColumnsRule() string {
	patterns := NoSoftDeleteColumnsPatterns()
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = `query.sql.matches(r"` + p + `")`
	}
	return strings.Join(parts, " || ")
}

// JoinFinding is a read of the active view of a parent on a detach edge in
// a position that drops the child rows whose parent is deleted, the trap of
// Java #309/#321 (ADR-0013 Karar 4).
type JoinFinding struct {
	// Where says how the parent is reached: "INNER JOIN active_x",
	// "FROM active_x", "IN (… active_x)", "CTE o", ….
	Where string
	Edge  Edge
}

// String describes the finding and its fix.
func (f JoinFinding) String() string {
	return fmt.Sprintf("%s on detach edge %s: reach the parent only by LEFT JOIN %s%s from the child", f.Where, f.Edge, ViewPrefix, f.Edge.Parent)
}

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlString       = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlToken        = regexp.MustCompile(`"[^"]*"|[a-z_][a-z0-9_$]*|[^\sa-z_]`)
)

// frameKind is what a parenthesis opens.
type frameKind int

const (
	frameTop     frameKind = iota // the statement itself
	frameDerived                  // a derived table or LATERAL subquery
	frameSemi                     // IN, EXISTS, ANY, SOME, ALL: a semi-join filter
	frameCTE                      // the body of a WITH query
	frameScalar                   // anything else: a scalar subquery, a function call, NOT EXISTS
)

// frame is one parenthesis level.
type frame struct {
	kind frameKind
	// join is the join kind of a derived frame in its parent level.
	join string
	// name is the name of a CTE frame.
	name string
	// clause is the last clause keyword seen at this level.
	clause string
}

// isClause reports the keywords that start a clause of a level.
func isClause(tok string) bool {
	switch tok {
	case "select", "from", "where", "group", "having", "order", "limit", "returning",
		"set", "values", "on", "using", "with", "into", "update":
		return true
	default:
		return false
	}
}

// semiJoin starts the description of a reference inside IN or EXISTS.
const semiJoin = "IN/EXISTS (…)"

// CheckJoins is the JOIN lint of ADR-0013 and ADR-0020. In a query that
// reads the child of a detach edge, the parent's active view (or a CTE built
// on it) may appear only on the nullable side of a LEFT (or FULL) join,
// directly or through a derived table or LATERAL subquery that is itself
// LEFT-joined, or in a scalar subquery (a select-list lookup, NOT EXISTS).
// Every other position drops children of deleted parents and is reported:
// FROM or an INNER, RIGHT, CROSS or NATURAL join (the parent may be on
// either side), a comma join, an inner-joined derived table or LATERAL
// subquery, a CTE used that way, and IN, EXISTS or ANY semi-joins. Quoted
// names count as plain names.
//
// It reads tokens, not a syntax tree; the limits are listed in ADR-0020
// Karar 10.
func CheckJoins(c *Catalog, sql string) []JoinFinding {
	tokens := tokenize(sql)

	// The child of an edge counts when the query produces its rows: it is
	// read (after FROM, JOIN or a comma in a FROM list) on a side that is not
	// nullable, outside scalar and semi-join subqueries. A child that is only
	// written (INSERT INTO, UPDATE), only LEFT-joined to its parent (a
	// parent-centric query) or only counted in a subquery is not dropped by
	// a join.
	read := map[string]bool{}
	walk(tokens, func(i int, f *frame, stack []*frame) {
		if !relationPosition(tokens, i, f) {
			return
		}
		if where, safe := position(tokens, i, stack); !safe && !strings.HasPrefix(where, semiJoin) {
			read[strings.TrimPrefix(tokens[i], ViewPrefix)] = true
		}
	})

	var out []JoinFinding
	for _, e := range c.Edges() {
		if e.Policy != Detach || !read[e.Child] {
			continue
		}
		out = append(out, checkEdge(tokens, e)...)
	}
	return out
}

// checkEdge finds the unsafe reads of e's parent in one query.
func checkEdge(tokens []string, e Edge) []JoinFinding {
	view := ViewPrefix + e.Parent
	aliases := map[string]bool{view: true}
	var out []JoinFinding
	walk(tokens, func(i int, f *frame, stack []*frame) {
		if !aliases[tokens[i]] || !relationPosition(tokens, i, f) {
			return
		}
		where, safe := position(tokens, i, stack)
		if safe {
			return
		}
		if strings.HasPrefix(where, "CTE ") {
			// The CTE now stands for the parent; its uses are checked
			// as the parent's.
			aliases[strings.TrimPrefix(where, "CTE ")] = true
			return
		}
		name := tokens[i]
		if name != view {
			name = "CTE " + name
		}
		out = append(out, JoinFinding{Where: where + " " + name, Edge: e})
	})
	return out
}

// position decides whether the relation at tokens[i] is on a nullable side.
// It returns a description of the unsafe position, "CTE <name>" when the
// reference is the body of a CTE (which then stands for the parent), and
// safe.
func position(tokens []string, i int, stack []*frame) (string, bool) {
	kind := localJoin(tokens, i)
	for level := len(stack) - 1; ; level-- {
		if kind == "left join" || kind == "full join" {
			return "", true
		}
		f := stack[level]
		switch f.kind {
		case frameTop:
			return strings.ToUpper(kind), false
		case frameScalar:
			return "", true
		case frameSemi:
			return semiJoin, false
		case frameCTE:
			return "CTE " + f.name, false
		case frameDerived:
			// The derived table sits in its parent level with its own
			// join kind; the decision moves up a level.
			kind = f.join
		}
	}
}

// localJoin returns how the relation at tokens[i] is reached in its own
// level: "from", "comma", or the kind of the JOIN before it.
func localJoin(tokens []string, i int) string {
	j := i - 1
	for j >= 0 && (tokens[j] == "." || tokens[j] == "public" || tokens[j] == "only") {
		j--
	}
	switch {
	case j < 0:
		return "from"
	case tokens[j] == "join":
		return joinKind(tokens[:j]) + " join"
	case tokens[j] == ",":
		return "comma join"
	default:
		return "from"
	}
}

// relationPosition reports whether tokens[i] names a relation that is read:
// after FROM, JOIN, a comma in a FROM list, or as the first token of a
// parenthesized join. A name followed by "." is a column qualifier.
func relationPosition(tokens []string, i int, f *frame) bool {
	if i+1 < len(tokens) && tokens[i+1] == "." {
		return false
	}
	j := i - 1
	for j >= 0 && (tokens[j] == "." || tokens[j] == "public" || tokens[j] == "only") {
		j--
	}
	if j < 0 {
		return false
	}
	switch tokens[j] {
	case "from", "join":
		return true
	case ",":
		return f.clause == "from" || f.clause == "on"
	case "(":
		return f.kind == frameDerived
	default:
		return false
	}
}

// walk calls fn for every identifier token with the current frame and the
// frame stack, tracking parentheses and clauses.
func walk(tokens []string, fn func(i int, f *frame, stack []*frame)) {
	stack := []*frame{{kind: frameTop}}
	for i, tok := range tokens {
		top := stack[len(stack)-1]
		switch {
		case tok == "(":
			stack = append(stack, openFrame(tokens, i, top))
		case tok == ")":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case tok == "join":
			top.clause = "from"
		case isClause(tok):
			top.clause = tok
		default:
			fn(i, top, stack)
		}
	}
}

// openFrame classifies the parenthesis at tokens[i] by what precedes it.
func openFrame(tokens []string, i int, parent *frame) *frame {
	prev := func(k int) string {
		if i-k < 0 {
			return ""
		}
		return tokens[i-k]
	}
	p := prev(1)
	if p == "lateral" {
		p = prev(2)
		i--
	}
	switch p {
	case "join":
		return &frame{kind: frameDerived, join: joinKind(tokens[:i-1]) + " join"}
	case "from":
		return &frame{kind: frameDerived, join: "from"}
	case ",":
		if parent.clause == "from" || parent.clause == "on" {
			return &frame{kind: frameDerived, join: "comma join"}
		}
		return &frame{kind: frameScalar}
	case "in", "exists", "any", "some", "all":
		if prev(2) == "not" {
			return &frame{kind: frameScalar}
		}
		return &frame{kind: frameSemi}
	case "as", "materialized":
		if name := cteName(tokens, i); name != "" && parent.clause == "with" {
			return &frame{kind: frameCTE, name: name}
		}
	}
	return &frame{kind: frameScalar}
}

// cteName returns the name of the CTE whose body opens at tokens[i]:
// `name [(cols)] AS [[NOT] MATERIALIZED] (`.
func cteName(tokens []string, i int) string {
	j := i - 1
	for j >= 0 && (tokens[j] == "materialized" || tokens[j] == "not") {
		j--
	}
	if j < 0 || tokens[j] != "as" {
		return ""
	}
	j--
	if j >= 0 && tokens[j] == ")" {
		depth := 0
	scan:
		for ; j >= 0; j-- {
			switch tokens[j] {
			case ")":
				depth++
			case "(":
				depth--
				if depth == 0 {
					break scan
				}
			}
		}
		j--
	}
	if j < 0 {
		return ""
	}
	return tokens[j]
}

// tokenize lower-cases the query, drops comments and string literals and
// splits it into tokens; a quoted identifier becomes its plain name.
func tokenize(sql string) []string {
	text := sqlBlockComment.ReplaceAllString(sql, " ")
	text = sqlLineComment.ReplaceAllString(text, " ")
	text = sqlString.ReplaceAllString(text, "''")
	text = strings.ToLower(text)
	tokens := sqlToken.FindAllString(text, -1)
	for i, tok := range tokens {
		if len(tok) >= 2 && tok[0] == '"' {
			tokens[i] = tok[1 : len(tok)-1]
		}
	}
	return tokens
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

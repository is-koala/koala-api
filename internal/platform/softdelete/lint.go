package softdelete

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
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
// which can write deleted_at or deletion_id. Only softdelete.Delete and
// softdelete.Restore may (ADR-0020 Karar 3, ADR-0027 §1). Quoted names,
// schema-qualified names and a comment between the parts of a form are
// caught; a comparison like `deletion_id = $1` after SET is reported too,
// because an application query has no reason to filter on the columns —
// its view already does. Reading them is allowed.
//
// The forms, one pattern each:
//
//  1. UPDATE … SET <column> = … (also ON CONFLICT DO UPDATE SET and the
//     UPDATE SET of a MERGE);
//  2. UPDATE … SET (…, <column>, …) = (…), the row-constructor form;
//  3. INSERT INTO … (…, <column>, …), a column list that names one;
//  4. INSERT (…, <column>, …), the INSERT of a MERGE, which has no INTO;
//  5. an INSERT INTO without a column list (… VALUES, … SELECT, … DEFAULT
//     VALUES, …): it writes every column of the relation positionally, the
//     two among them, and no pattern can see a name that is not written;
//  6. the same INSERT without a column list inside a MERGE.
//
// Patterns 5 and 6 reject a statement shape rather than a named column:
// an application query lists the columns it writes.
func NoSoftDeleteColumnsPatterns() []string {
	// \x22 is the double quote: a raw CEL string cannot contain one.
	const column = `\x22?\b(?:deleted_at|deletion_id)\b\x22?`
	// name is one identifier, quoted or not.
	const name = `\x22?[a-z_][a-z0-9_$]*\x22?`
	// gap is whitespace and comments: a form may not be split by one.
	const gap = `(?:\s|/\*.*?\*/|--[^\n]*\n)*`
	// headless is an INSERT whose relation is followed by a keyword
	// instead of a column list.
	const headless = `\s+(?:values|select|table|with|overriding|default\s+values)\b`
	return []string{
		`(?is)\bupdate\b.*\bset\b.*` + column + gap + `=`,
		`(?is)\bupdate\b.*\bset\b.*\([^()]*` + column + `[^()]*\)` + gap + `=`,
		`(?is)\binsert\s+into\s+[^(]*\([^)]*` + column,
		`(?is)\binsert` + gap + `\([^)]*` + column,
		`(?is)\binsert\s+into\s+(?:` + name + gap + `\.` + gap + `)?` + name +
			`(?:\s+as\s+` + name + `)?` + headless,
		`(?is)\bthen\s+insert` + headless,
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
	// Hint is the advice of a finding that is not about the join's
	// position: the nullable-side detector and the exemption marker
	// (ADR-0027 §5). It is empty for a position finding, which states its
	// own fix.
	Hint string
}

// String describes the finding and its fix.
func (f JoinFinding) String() string {
	if f.Hint != "" {
		return f.Where + ": " + f.Hint
	}
	return fmt.Sprintf("%s on detach edge %s: reach the parent only by LEFT JOIN %s%s from the child", f.Where, f.Edge, ViewPrefix, f.Edge.Parent)
}

// Exemption is the comment that exempts one detach parent from the
// nullable-side detector of ADR-0027 §5: the query destroys the nullable
// side on purpose and fails closed.
//
//	-- softdelete:inner-by-design <parent table>: <reason>
//
// It is a comment line of the query (the leading block, so sqlc carries it
// into the generated doc comment), it names the parent table or its active
// view, and it needs a reason of at least ExemptionReasonLen characters. A
// marker without a reason or a parent, and a marker that suppresses
// nothing, are findings of their own: there is no silent exemption and no
// stale one.
const Exemption = "softdelete:inner-by-design"

// ExemptionReasonLen is the shortest accepted reason, as for
// x-resource-reason in api/ruleset.yaml.
const ExemptionReasonLen = 20

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlString       = regexp.MustCompile(`(?s)'(?:[^']|'')*'`)
	sqlToken        = regexp.MustCompile(`"[^"]*"|[a-z_][a-z0-9_$]*|[^\sa-z_]`)
	plainName       = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)
	exemptionMarker = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*` + regexp.QuoteMeta(Exemption) +
		`[ \t]*(?:([a-z_][a-z0-9_$]*)[ \t]*:)?[ \t]*(.*)$`)
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
// A LEFT join whose nullable side a WHERE (or HAVING) predicate destroys is
// reported too: written as an outer join, it behaves like an inner one
// (ADR-0027 §5). A query that means it says so with the Exemption marker.
//
// It reads tokens, not a syntax tree; the limits are listed in ADR-0020
// Karar 10 and ADR-0027 §4.
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

	exempt, out := exemptions(sql)
	used := map[string]bool{}
	for _, e := range c.Edges() {
		if e.Policy != Detach || !read[e.Child] {
			continue
		}
		out = append(out, checkEdge(tokens, e)...)
		out = append(out, checkNullableSide(tokens, e, exempt, used)...)
	}
	for _, parent := range slices.Sorted(maps.Keys(exempt)) {
		if used[parent] {
			continue
		}
		out = append(out, JoinFinding{
			Where: Exemption + " " + parent,
			Hint:  "it exempts nothing in this query; remove it or name the detach parent whose nullable side the query destroys",
		})
	}
	return out
}

// exemptions reads the Exemption markers of one query and returns the
// parent tables they exempt, with the findings: a marker that names no
// parent and one whose reason is too short. Such a marker exempts nothing,
// so the join it was meant for is reported as well.
func exemptions(sql string) (map[string]bool, []JoinFinding) {
	out := map[string]bool{}
	var problems []JoinFinding
	for _, m := range exemptionMarker.FindAllStringSubmatch(sql, -1) {
		parent, reason := m[1], strings.TrimSpace(m[2])
		if parent == "" {
			problems = append(problems, JoinFinding{
				Where: Exemption,
				Hint:  "name the detach parent table it exempts, then a colon and the reason",
			})
			continue
		}
		if len([]rune(reason)) < ExemptionReasonLen {
			problems = append(problems, JoinFinding{
				Where: Exemption + " " + parent,
				Hint:  fmt.Sprintf("state why the nullable side may be destroyed, in at least %d characters", ExemptionReasonLen),
			})
			continue
		}
		out[strings.TrimPrefix(parent, ViewPrefix)] = true
	}
	return out, problems
}

// checkNullableSide reports the LEFT (or FULL) joined reads of e's parent
// whose nullable side a WHERE or HAVING predicate destroys: the join keeps
// the children of deleted parents, the predicate then drops them, so the
// pair behaves like an inner join. A predicate that only tests for NULL (IS
// NULL, IS NOT DISTINCT FROM) keeps the nullable side and is not a finding.
// An Exemption marker naming e's parent suppresses the finding and is
// recorded as used.
func checkNullableSide(tokens []string, e Edge, exempt map[string]bool, used map[string]bool) []JoinFinding {
	view := ViewPrefix + e.Parent
	// The names the parent is referred to by where it is nullable.
	nullable := map[string]bool{}
	walk(tokens, func(i int, f *frame, _ []*frame) {
		if tokens[i] != view || !relationPosition(tokens, i, f) {
			return
		}
		if k := localJoin(tokens, i); k == "left join" || k == "full join" {
			nullable[aliasAt(tokens, i)] = true
		}
	})
	if len(nullable) == 0 {
		return nil
	}
	var out []JoinFinding
	done := map[string]bool{}
	walk(tokens, func(i int, f *frame, _ []*frame) {
		if f.clause != "where" && f.clause != "having" {
			return
		}
		name := tokens[i]
		if !nullable[name] || done[name] || i+2 >= len(tokens) || tokens[i+1] != "." || nullSafe(tokens, i+2) {
			return
		}
		done[name] = true
		if exempt[e.Parent] {
			used[e.Parent] = true
			return
		}
		out = append(out, JoinFinding{
			Edge:  e,
			Where: fmt.Sprintf("LEFT JOIN %s with %s.%s in %s", view, name, tokens[i+2], strings.ToUpper(f.clause)),
			Hint: "the predicate drops the rows whose parent is NULL, so the outer join behaves like an inner one on detach edge " +
				e.String() + ": keep NULL and decide in Go, or write `-- " + Exemption + " " + e.Parent + ": <reason>` in the query's comment",
		})
	})
	return out
}

// nullSafe reports whether the predicate that starts at the column token
// tokens[i] keeps the NULL rows: IS NULL and IS NOT DISTINCT FROM are the
// two forms that do not drop the nullable side.
func nullSafe(tokens []string, i int) bool {
	switch {
	case i+2 < len(tokens) && tokens[i+1] == "is" && tokens[i+2] == "null":
		return true
	case i+3 < len(tokens) && tokens[i+1] == "is" && tokens[i+2] == "not" && tokens[i+3] == "distinct":
		return true
	default:
		return false
	}
}

// aliasAt returns the name the relation at tokens[i] is referred to by: its
// alias when it has one, its own name otherwise.
func aliasAt(tokens []string, i int) string {
	j := i + 1
	if j < len(tokens) && tokens[j] == "as" {
		j++
	}
	if j < len(tokens) && plainName.MatchString(tokens[j]) && !aliasStop[tokens[j]] {
		return tokens[j]
	}
	return tokens[i]
}

// aliasStop holds the keywords that may follow a relation without being its
// alias.
var aliasStop = map[string]bool{
	"on": true, "using": true, "where": true, "left": true, "right": true,
	"full": true, "inner": true, "cross": true, "natural": true, "join": true,
	"group": true, "having": true, "order": true, "limit": true, "offset": true,
	"fetch": true, "for": true, "union": true, "except": true, "intersect": true,
	"returning": true, "window": true, "with": true, "and": true, "or": true,
	"tablesample": true, "lateral": true, "set": true, "values": true, "select": true,
	"from": true, "into": true,
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

// tokenize lower-cases the query, blanks comments and string literals and
// splits it into tokens; a quoted identifier becomes its plain name.
func tokenize(sql string) []string {
	text := sqlBlockComment.ReplaceAllStringFunc(sql, blank)
	text = sqlLineComment.ReplaceAllStringFunc(text, blank)
	text = sqlString.ReplaceAllStringFunc(text, blank)
	text = strings.ToLower(text)
	tokens := sqlToken.FindAllString(text, -1)
	for i, tok := range tokens {
		if len(tok) >= 2 && tok[0] == '"' {
			tokens[i] = tok[1 : len(tok)-1]
		}
	}
	return tokens
}

// blank replaces every byte of s but its newlines with a space, so the text
// keeps its offsets and its line numbering.
func blank(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
	return string(b)
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

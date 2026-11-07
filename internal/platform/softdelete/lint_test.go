package softdelete_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// The JOIN lint is red on every read of a detach edge's parent that drops
// the children of deleted parents, and green on the safe forms (ADR-0013
// Karar 4, ADR-0020 Karar 10). Example edges:
// example_comments.author_id -> example_owners is detach,
// example_projects.owner_id -> example_owners is cascade.
func TestCheckJoins(t *testing.T) {
	t.Parallel()

	const (
		c = "active_example_comments"
		o = "active_example_owners"
	)
	red := map[string]string{
		"plain join":            "SELECT c.id FROM " + c + " c JOIN " + o + " o ON o.id = c.author_id",
		"inner join, lowercase": "select c.id from " + c + " c inner join " + o + " o on o.id = c.author_id",
		"right join":            "SELECT c.id FROM " + c + " c RIGHT JOIN " + o + " o ON o.id = c.author_id",
		"cross join":            "SELECT c.id FROM " + c + " c CROSS JOIN " + o + " o",
		"natural join":          "SELECT c.id FROM " + c + " c NATURAL JOIN " + o,
		"schema-qualified":      "SELECT c.id FROM " + c + " c JOIN public." + o + " o ON o.id = c.author_id",
		"quoted names":          `SELECT c.id FROM "` + c + `" c JOIN "public"."` + o + `" o ON o.id = c.author_id`,
		"multi-line":            "SELECT c.id\nFROM " + c + " c\n  JOIN\n  " + o + " o\n  ON o.id = c.author_id",
		"child named as base":   "SELECT count(*) FROM example_comments c JOIN " + o + " o ON o.id = c.author_id",
		"parent in FROM, inner join to the child": "SELECT c.id FROM " + o + " o JOIN " + c + " c ON c.author_id = o.id",
		"comma join":                    "SELECT c.id FROM " + c + " c, " + o + " o WHERE o.id = c.author_id",
		"inner-joined derived table":    "SELECT c.id FROM " + c + " c JOIN (SELECT id FROM " + o + ") o ON o.id = c.author_id",
		"derived table in FROM":         "SELECT x.id FROM (SELECT c.id FROM " + c + " c JOIN " + o + " o ON o.id = c.author_id) x",
		"inner LATERAL":                 "SELECT c.id, x.name FROM " + c + " c JOIN LATERAL (SELECT name FROM " + o + " o WHERE o.id = c.author_id) x ON true",
		"comma LATERAL":                 "SELECT c.id, x.name FROM " + c + " c, LATERAL (SELECT name FROM " + o + " o WHERE o.id = c.author_id) x",
		"CTE inner-joined":              "WITH owners AS (SELECT id FROM " + o + ") SELECT c.id FROM " + c + " c JOIN owners ON owners.id = c.author_id",
		"materialized CTE with columns": "WITH owners (oid) AS MATERIALIZED (SELECT id FROM " + o + ") SELECT c.id FROM " + c + " c JOIN owners ON owners.oid = c.author_id",
		"IN semi-join":                  "SELECT c.id FROM " + c + " c WHERE c.author_id IN (SELECT id FROM " + o + ")",
		"EXISTS semi-join":              "SELECT c.id FROM " + c + " c WHERE EXISTS (SELECT FROM " + o + " o WHERE o.id = c.author_id)",
		"ANY semi-join":                 "SELECT c.id FROM " + c + " c WHERE c.author_id = ANY (SELECT id FROM " + o + ")",
	}
	green := map[string]string{
		"left join":               "SELECT c.id FROM " + c + " c LEFT JOIN " + o + " o ON o.id = c.author_id",
		"left outer join":         "SELECT c.id FROM " + c + " c LEFT OUTER JOIN " + o + " o ON o.id = c.author_id",
		"full join":               "SELECT c.id FROM " + c + " c FULL JOIN " + o + " o ON o.id = c.author_id",
		"left-joined derived":     "SELECT c.id FROM " + c + " c LEFT JOIN (SELECT id, name FROM " + o + ") o ON o.id = c.author_id",
		"left LATERAL":            "SELECT c.id, x.name FROM " + c + " c LEFT JOIN LATERAL (SELECT name FROM " + o + " o WHERE o.id = c.author_id) x ON true",
		"CTE left-joined":         "WITH owners AS (SELECT id FROM " + o + ") SELECT c.id FROM " + c + " c LEFT JOIN owners ON owners.id = c.author_id",
		"scalar lookup":           "SELECT c.id, (SELECT name FROM " + o + " o WHERE o.id = c.author_id) FROM " + c + " c",
		"NOT EXISTS":              "SELECT c.id FROM " + c + " c WHERE NOT EXISTS (SELECT FROM " + o + " o WHERE o.id = c.author_id)",
		"parent-centric":          "SELECT o.id, c.id FROM " + o + " o LEFT JOIN " + c + " c ON c.author_id = o.id",
		"count per parent":        "SELECT o.id, (SELECT count(*) FROM " + c + " c WHERE c.author_id = o.id) FROM " + o + " o",
		"child only written":      "INSERT INTO " + c + " (id, author_id, body) SELECT $1, o.id, $2 FROM " + o + " o WHERE o.id = $3",
		"cascade edge":            "SELECT p.id FROM active_example_projects p JOIN " + o + " o ON o.id = p.owner_id",
		"child not in query":      "SELECT n.id FROM active_example_notes n JOIN " + o + " o ON true",
		"qualifier only":          "SELECT " + o + ".name FROM " + c + " c LEFT JOIN " + o + " ON " + o + ".id = c.author_id",
		"join in a comment":       "SELECT c.id FROM " + c + " c -- JOIN " + o + " o\n",
		"join in a block comment": "SELECT c.id FROM " + c + " c /* JOIN " + o + " */",
		"join in a string":        "SELECT 'JOIN " + o + "' FROM " + c + " c",
	}

	cat := exampleCatalog(t)
	for name, sql := range red {
		t.Run("red/"+name, func(t *testing.T) {
			t.Parallel()
			got := softdelete.CheckJoins(cat, sql)
			if len(got) != 1 {
				t.Fatalf("findings = %v, want 1", got)
			}
			if e := got[0].Edge; e.Policy != softdelete.Detach || e.Child != "example_comments" {
				t.Errorf("finding %s is not on the detach edge", got[0])
			}
		})
	}
	for name, sql := range green {
		t.Run("green/"+name, func(t *testing.T) {
			t.Parallel()
			if got := softdelete.CheckJoins(cat, sql); len(got) != 0 {
				t.Fatalf("findings = %v, want none", got)
			}
		})
	}
}

// The nullable-side detector of ADR-0027 §5: a LEFT (or FULL) join of a
// detach edge's parent whose NULL rows a WHERE or HAVING predicate drops
// behaves like an inner join and is a finding, unless the query carries the
// inner-by-design marker with the parent's name and a reason.
func TestCheckJoinsNullableSide(t *testing.T) {
	t.Parallel()

	const (
		c      = "active_example_comments"
		o      = "active_example_owners"
		reason = "the answer is fail-closed: a comment of a deleted owner must not be read"
	)
	// mark is the exemption marker for the detach parent of the edge.
	const mark = "-- " + softdelete.Exemption + " example_owners: " + reason + "\n"
	// join reads the child and LEFT-joins its parent.
	const join = "SELECT c.id, o.name\nFROM " + c + " c\nLEFT JOIN " + o + " o ON o.id = c.author_id\n"

	red := map[string]string{
		"equality on the parent":   join + "WHERE o.name = $1",
		"IS NOT NULL":              join + "WHERE o.id IS NOT NULL",
		"HAVING":                   join + "GROUP BY c.id, o.name\nHAVING o.name = $1",
		"alias with AS":            "SELECT c.id FROM " + c + " c LEFT JOIN " + o + " AS o ON o.id = c.author_id WHERE o.name = $1",
		"no alias":                 "SELECT c.id FROM " + c + " c LEFT JOIN " + o + " ON " + o + ".id = c.author_id WHERE " + o + ".name = $1",
		"full join":                "SELECT c.id FROM " + c + " c FULL JOIN " + o + " o ON o.id = c.author_id WHERE o.name = $1",
		"marker on another parent": "-- " + softdelete.Exemption + " example_projects: " + reason + "\n" + join + "WHERE o.name = $1",
		"marker without a parent":  "-- " + softdelete.Exemption + " " + reason + "\n" + join + "WHERE o.name = $1",
		"marker with a short why":  "-- " + softdelete.Exemption + " example_owners: short\n" + join + "WHERE o.name = $1",
	}
	green := map[string]string{
		"no predicate on the parent": join + "WHERE c.body = $1",
		"IS NULL":                    join + "WHERE o.id IS NULL",
		"IS NOT DISTINCT FROM":       join + "WHERE o.name IS NOT DISTINCT FROM $1",
		"predicate in ON":            "SELECT c.id FROM " + c + " c LEFT JOIN " + o + " o ON o.id = c.author_id AND o.name = $1",
		"ORDER BY on the parent":     join + "WHERE c.body = $1 ORDER BY o.name",
		"cascade edge":               "SELECT p.id FROM active_example_projects p LEFT JOIN " + o + " o ON o.id = p.owner_id WHERE o.name = $1",
		"exempted":                   mark + join + "WHERE o.name = $1",
		"exempted, view name":        "-- " + softdelete.Exemption + " " + o + ": " + reason + "\n" + join + "WHERE o.name = $1",
		"exempted, indented marker":  "   " + mark + join + "WHERE o.name = $1",
	}

	cat := exampleCatalog(t)
	for name, sql := range red {
		t.Run("red/"+name, func(t *testing.T) {
			t.Parallel()
			got := softdelete.CheckJoins(cat, sql)
			if len(got) == 0 {
				t.Fatal("no finding, want at least one")
			}
			if !strings.Contains(got[0].String(), softdelete.Exemption) {
				t.Errorf("finding %s does not name the exemption marker", got[0])
			}
		})
	}
	for name, sql := range green {
		t.Run("green/"+name, func(t *testing.T) {
			t.Parallel()
			if got := softdelete.CheckJoins(cat, sql); len(got) != 0 {
				t.Fatalf("findings = %v, want none", got)
			}
		})
	}

	// A marker that suppresses nothing is a finding of its own: a stale
	// exemption does not stay behind silently.
	t.Run("red/unused marker", func(t *testing.T) {
		t.Parallel()
		got := softdelete.CheckJoins(cat, mark+join+"WHERE c.body = $1")
		if len(got) != 1 || !strings.Contains(got[0].String(), "exempts nothing") {
			t.Fatalf("findings = %v, want one about an exemption that suppresses nothing", got)
		}
	})
	// The marker does not excuse an inner join (ADR-0020 Karar 10).
	t.Run("red/marker does not excuse an inner join", func(t *testing.T) {
		t.Parallel()
		sql := mark + "SELECT c.id FROM " + c + " c JOIN " + o + " o ON o.id = c.author_id"
		got := softdelete.CheckJoins(cat, sql)
		if len(got) != 2 {
			t.Fatalf("findings = %v, want the inner join and the unused marker", got)
		}
		if e := got[0].Edge; e.Policy != softdelete.Detach || e.Child != "example_comments" {
			t.Errorf("finding %s is not the inner join on the detach edge", got[0])
		}
	})
}

// The no-base-table pattern is RE2, the regexp syntax of both Go and CEL's
// matches(), so this test exercises the expression sqlc vet evaluates.
// scripts/sqlc-vet-probe.sh proves the rule fires inside sqlc itself.
func TestNoBaseTablePattern(t *testing.T) {
	t.Parallel()

	re := regexp.MustCompile(softdelete.NoBaseTablePattern(exampleCatalog(t)))
	tests := map[string]bool{
		"SELECT id FROM example_owners":                                             true,
		"select id from EXAMPLE_OWNERS":                                             true,
		"SELECT id FROM public.example_owners":                                      true,
		"SELECT p.id FROM active_example_projects p\nJOIN example_owners o ON true": true,
		"INSERT INTO example_notes (id) VALUES ($1)":                                true,
		"UPDATE example_tasks SET name = $1":                                        true,
		"DELETE FROM ONLY example_tasks":                                            true,
		"TRUNCATE TABLE example_tasks":                                              true,
		"DELETE FROM active_example_notes n USING example_owners o WHERE true":      true,
		"SELECT id FROM active_example_owners":                                      false,
		"UPDATE active_example_tasks SET name = $1":                                 false,
		"SELECT id FROM example_owners_archive":                                     false,
		"SELECT example_owners FROM deletions":                                      false,
		"INSERT INTO deletions (id) VALUES ($1)":                                    false,
	}
	for sql, want := range tests {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			if got := re.MatchString(sql); got != want {
				t.Fatalf("match = %v, want %v", got, want)
			}
		})
	}
}

// The no-soft-delete-columns patterns, as sqlc vet evaluates them (OR).
func TestNoSoftDeleteColumnsPatterns(t *testing.T) {
	t.Parallel()

	var res []*regexp.Regexp
	for _, p := range softdelete.NoSoftDeleteColumnsPatterns() {
		res = append(res, regexp.MustCompile(p))
	}
	tests := map[string]bool{
		"UPDATE active_example_tasks SET deleted_at = now() WHERE id = $1":                                    true,
		"update active_example_tasks set name = $2,\n  deletion_id = $3 where id = $1":                        true,
		`UPDATE active_example_tasks SET "deletion_id" = $2 WHERE id = $1`:                                    true,
		"INSERT INTO active_example_notes (id, body, deleted_at) VALUES ($1, $2, $3)":                         true,
		"INSERT INTO active_example_notes (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET deletion_id = NULL": true,
		// The forms ADR-0027 §1 added.
		"UPDATE active_example_notes SET (deleted_at, deletion_id) = (now(), $1) WHERE id = $2":                                   true,
		`UPDATE active_example_notes SET ("deleted_at", "deletion_id") = (now(), $1) WHERE id = $2`:                               true,
		"UPDATE active_example_notes SET body = $2, (deleted_at, deletion_id) = (now(), $3)":                                      true,
		"UPDATE active_example_tasks SET deleted_at /* a comment */ = now() WHERE id = $1":                                        true,
		"UPDATE active_example_tasks SET deleted_at -- a comment\n = now() WHERE id = $1":                                         true,
		"INSERT INTO active_example_notes VALUES ($1, $2, $3, $4, $5)":                                                            true,
		"insert into active_example_notes\nvalues ($1, $2, $3, $4, $5)":                                                           true,
		"INSERT INTO public.active_example_notes VALUES ($1, $2, $3, $4, $5)":                                                     true,
		`INSERT INTO "active_example_notes" VALUES ($1, $2, $3, $4, $5)`:                                                          true,
		"INSERT INTO active_example_notes AS n VALUES ($1, $2, $3, $4, $5)":                                                       true,
		"INSERT INTO active_example_notes SELECT * FROM active_example_notes WHERE id = $1":                                       true,
		"INSERT INTO active_example_notes DEFAULT VALUES":                                                                         true,
		"MERGE INTO active_example_notes n USING x ON x.id = n.id\nWHEN NOT MATCHED THEN INSERT (id, deleted_at) VALUES ($1, $2)": true,
		"MERGE INTO active_example_notes n USING x ON x.id = n.id\nWHEN NOT MATCHED THEN INSERT VALUES ($1, $2, $3, $4, $5)":      true,
		"MERGE INTO active_example_notes n USING x ON x.id = n.id\nWHEN MATCHED THEN UPDATE SET deleted_at = now()":               true,

		"SELECT id, deleted_at FROM active_example_notes":                 false,
		"UPDATE active_example_tasks SET name = $2 WHERE id = $1":         false,
		"INSERT INTO active_example_notes (id, body) VALUES ($1, $2)":     false,
		"UPDATE active_example_tasks SET deleted_note = $2 WHERE id = $1": false,
		// A column list keeps every INSERT shape green.
		"INSERT INTO active_example_notes (id, body)\nSELECT $1, body FROM active_example_notes WHERE id = $2": false,
		"INSERT INTO public.active_example_notes (id, body) VALUES ($1, $2)":                                   false,
		"INSERT INTO active_example_notes AS n (id, body) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING":         false,
		"UPDATE active_example_notes SET (body, created_at) = ($2, $3) WHERE id = $1":                          false,
		// Reading the columns in a subquery is not a write.
		"UPDATE active_example_notes SET body = (SELECT body FROM active_example_notes WHERE deleted_at IS NULL) WHERE id = $1": false,
	}
	for sql, want := range tests {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()
			got := false
			for _, re := range res {
				got = got || re.MatchString(sql)
			}
			if got != want {
				t.Fatalf("match = %v, want %v", got, want)
			}
		})
	}
	rule := softdelete.NoSoftDeleteColumnsRule()
	// Each pattern sits in its own raw CEL string, which cannot contain a
	// double quote: the patterns spell it \x22.
	if strings.Count(rule, "query.sql.matches(r\"") != len(res) || strings.Count(rule, `"`) != 2*len(res) {
		t.Errorf("rule %s does not wrap every pattern in its own raw CEL string", rule)
	}
}

func TestNoBaseTableRule(t *testing.T) {
	t.Parallel()

	empty, err := softdelete.ParseCatalog([]byte("tables:\n  ledger:\n    class: M1\n    reason: evidence\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := softdelete.NoBaseTableRule(empty); got != "false" {
		t.Errorf("rule without soft-deleted tables = %q, want false", got)
	}
	want := `query.sql.matches(r"` + softdelete.NoBaseTablePattern(exampleCatalog(t)) + `")`
	if got := softdelete.NoBaseTableRule(exampleCatalog(t)); got != want {
		t.Errorf("rule = %s, want %s", got, want)
	}
}

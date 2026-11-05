package softdelete_test

import (
	"regexp"
	"testing"

	"github.com/is-koala/koala-api/internal/platform/softdelete"
)

// The JOIN lint is red on every non-LEFT join to the parent of a detach
// edge and green on the safe forms (ADR-0013 Karar 4). Example edges:
// example_comments.author_id -> example_owners is detach,
// example_projects.owner_id -> example_owners is cascade.
func TestCheckJoins(t *testing.T) {
	t.Parallel()

	cat := exampleCatalog(t)
	tests := map[string]struct {
		sql  string
		want int
	}{
		"plain join":              {"SELECT c.id FROM active_example_comments c JOIN active_example_owners o ON o.id = c.author_id", 1},
		"inner join, lowercase":   {"select c.id from active_example_comments c inner join active_example_owners o on o.id = c.author_id", 1},
		"right join":              {"SELECT c.id FROM active_example_comments c RIGHT JOIN active_example_owners o ON o.id = c.author_id", 1},
		"cross join":              {"SELECT c.id FROM active_example_comments c CROSS JOIN active_example_owners o", 1},
		"schema-qualified":        {"SELECT c.id FROM active_example_comments c JOIN public.active_example_owners o ON o.id = c.author_id", 1},
		"multi-line":              {"SELECT c.id\nFROM active_example_comments c\n  JOIN\n  active_example_owners o\n  ON o.id = c.author_id", 1},
		"child named as base":     {"SELECT count(*) FROM example_comments c JOIN active_example_owners o ON o.id = c.author_id", 1},
		"left join":               {"SELECT c.id FROM active_example_comments c LEFT JOIN active_example_owners o ON o.id = c.author_id", 0},
		"left outer join":         {"SELECT c.id FROM active_example_comments c LEFT OUTER JOIN active_example_owners o ON o.id = c.author_id", 0},
		"full join":               {"SELECT c.id FROM active_example_comments c FULL JOIN active_example_owners o ON o.id = c.author_id", 0},
		"cascade edge":            {"SELECT p.id FROM active_example_projects p JOIN active_example_owners o ON o.id = p.owner_id", 0},
		"child not in query":      {"SELECT n.id FROM active_example_notes n JOIN active_example_owners o ON true", 0},
		"join in a comment":       {"SELECT c.id FROM active_example_comments c -- JOIN active_example_owners o\n", 0},
		"join in a block comment": {"SELECT c.id FROM active_example_comments c /* JOIN active_example_owners */", 0},
		"join in a string":        {"SELECT 'JOIN active_example_owners' FROM active_example_comments c", 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := softdelete.CheckJoins(cat, tt.sql)
			if len(got) != tt.want {
				t.Fatalf("findings = %v, want %d", got, tt.want)
			}
			for _, f := range got {
				if f.Edge.Policy != softdelete.Detach || f.Edge.Child != "example_comments" {
					t.Errorf("finding %s is not on the detach edge", f)
				}
			}
		})
	}
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
		"MERGE INTO example_tasks t USING example_projects p ON true":               true,
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

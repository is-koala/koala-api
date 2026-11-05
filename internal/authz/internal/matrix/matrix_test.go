package matrix_test

import (
	"strings"
	"testing"

	"github.com/is-koala/koala-api/internal/authz/internal/matrix"
)

const validDoc = `version: 0
status: draft
engines:
  - [go, openfga]
  - go_only
  - skip
basis:
  worker: "atamanın işçisi (job_assignment#worker)"
  public: "herkese açık ([user:*] ya da kimliksiz)"
actions:
  job_assignment.read: "job_assignment#can_read"
  image.delete: "go:owner"
fixtures:
  subjects:
    anonymous: {type: anonymous}
    user:u_worker: {type: user, note: "işçi"}
  objects:
    job_assignment:asg: {type: job_assignment, attrs: {"status": "CONFIRMED", "payment_status": null}}
    company:co: {type: company, attrs: {}}
    user:u_worker: {type: user, attrs: {"is_employer": false}}
  tuples:
    - "job_assignment:asg#worker@user:u_worker"
    - "job_assignment:asg#viewer@user:*"
    - "job_assignment:asg#reviewer@company:co#member"
cases:
  - id: AZM-001
    pair: C01
    action: job_assignment.read
    subject: "user:u_worker"
    object: "job_assignment:asg"
    expect: allow
    basis: worker
    java: allow
    engine: [go, openfga]
    inventory: [AZ-001]
    source: []
  - id: AZM-002
    pair: C01
    action: image.delete
    subject: "anonymous"
    object: "company:co"
    expect: deny
    conceal: true
    java: deny
    java_conceal: true
    engine: go_only
    note: "x"
    inventory: [AZ-002]
    source: ["a#b"]
`

func TestParse(t *testing.T) {
	t.Parallel()
	m, err := matrix.Parse([]byte(validDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Cases) != 2 || m.Cases[0].Engine != matrix.EngineBoth || m.Cases[1].Engine != matrix.EngineGoOnly {
		t.Fatalf("cases: %+v", m.Cases)
	}
	if !m.Cases[0].Allowed() || m.Cases[1].Conceal == nil || !*m.Cases[1].Conceal {
		t.Fatalf("decisions: %+v", m.Cases)
	}
	if typ, rel, ok := m.Relation("job_assignment.read"); !ok || typ != "job_assignment" || rel != "can_read" {
		t.Errorf("Relation: %s %s %v", typ, rel, ok)
	}
	if _, _, ok := m.Relation("image.delete"); ok {
		t.Errorf("Relation of a go: rule")
	}
	if rel, ok := m.BasisRelation("worker"); !ok || rel != "job_assignment#worker" {
		t.Errorf("BasisRelation(worker) = %q %v", rel, ok)
	}
	if _, ok := m.BasisRelation("public"); ok {
		t.Errorf("BasisRelation(public) names a relation")
	}
	if len(m.Tuples()) != 3 {
		t.Errorf("tuples: %v", m.Tuples())
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		from, to string
	}{
		{"unknown field", "    note: \"x\"\n", "    notes: \"x\"\n"},
		{"duplicate key", "    basis: worker\n", "    basis: worker\n    basis: worker\n"},
		{"second document", "", "---\nversion: 1\n"},
		{"undeclared subject", `subject: "user:u_worker"`, `subject: "user:u_ghost"`},
		{"undeclared object", `object: "job_assignment:asg"`, `object: "job_assignment:ghost"`},
		{"undeclared tuple object", `"job_assignment:asg#worker@`, `"job_assignment:ghost#worker@`},
		{"undeclared tuple user", `#worker@user:u_worker"`, `#worker@user:u_ghost"`},
		{"undeclared userset", `@company:co#member"`, `@company:ghost#member"`},
		{"wildcard of undeclared type", `#viewer@user:*"`, `#viewer@staff:*"`},
		{"fixture type mismatch", `company:co: {type: company`, `company:co: {type: job`},
		{"unknown engine", "engine: go_only", "engine: go"},
		{"engine mapping", "engine: go_only", "engine: {go: true}"},
		{"unknown action", "action: image.delete", "action: image.upload"},
		{"unknown basis", "basis: worker\n", "basis: self\n"},
		{"deny without conceal", "    conceal: true\n", ""},
		{"bad expect", "expect: allow", "expect: maybe"},
		{"duplicate case", "id: AZM-002", "id: AZM-001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var doc string
			if tt.from == "" {
				doc = validDoc + tt.to
			} else {
				if !strings.Contains(validDoc, tt.from) {
					t.Fatalf("test bug: %q not in the document", tt.from)
				}
				doc = strings.Replace(validDoc, tt.from, tt.to, 1)
			}
			if _, err := matrix.Parse([]byte(doc)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

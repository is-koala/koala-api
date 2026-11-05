package authz

import "strings"

// Step is one relation lookup on the way to a grant: the relation of an
// object, for example staff_role:finance_officer#assignee.
type Step struct {
	Object   Object
	Relation string
}

// String is "type:id#relation".
func (s Step) String() string { return s.Object.String() + "#" + s.Relation }

// Path is one chain of lookups from the asked relation to the tuple that
// granted it. Steps[0] is the asked object#relation; the last step holds
// the granting tuple and Grant is that tuple's user side: the subject
// itself or a wildcard ("user:*"). The last step and Grant together are
// the granting tuple.
type Path struct {
	Steps []Step
	Grant TupleUser
}

// String is the steps joined by " > " followed by "@" and the grant:
// "job_assignment:a#can_read > job_assignment:a#worker@user:<id>".
func (p Path) String() string {
	parts := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		parts[i] = s.String()
	}
	return strings.Join(parts, " > ") + "@" + p.Grant.String()
}

// Basis says why a decision was allowed (plan §5.3: "worker", "editor
// from job", "staff_role:FINANCE_OFFICER"). It is deterministic: the
// evaluator visits rewrites in model order and tuples in sorted order, so
// the same data gives the same Basis. A union contributes the first
// operand that grants; an intersection ("and") contributes one path per
// operand, all of which hold; an exclusion ("but not") contributes its
// base.
type Basis []Path

// String is the audit form: the paths joined by " & ".
func (b Basis) String() string {
	parts := make([]string, len(b))
	for i, p := range b {
		parts[i] = p.String()
	}
	return strings.Join(parts, " & ")
}

// Relations lists "type#relation" of every step of every path, in order.
func (b Basis) Relations() []string {
	var out []string
	for _, p := range b {
		for _, s := range p.Steps {
			out = append(out, s.Object.Type+"#"+s.Relation)
		}
	}
	return out
}

package softdelete

import (
	"slices"
	"testing"
)

// Restore locks the parents of the rows it reopens in one order for every
// restore: grouped by parent table, the tables in name order (ADR-0027 §6).
// Edges() sorts by child, which would order the parent tables by their
// children's names instead.
func TestLockOrderGroupsByParent(t *testing.T) {
	t.Parallel()

	edges := []Edge{
		{Child: "b_children", Column: "z_id", Parent: "z_parents", Policy: Cascade},
		{Child: "a_children", Column: "m_id", Parent: "m_parents", Policy: Restrict},
		{Child: "b_children", Column: "m_id", Parent: "m_parents", Policy: Cascade},
		{Child: "a_children", Column: "z_id", Parent: "z_parents", Policy: Restrict},
	}
	want := []Edge{
		{Child: "a_children", Column: "m_id", Parent: "m_parents", Policy: Restrict},
		{Child: "b_children", Column: "m_id", Parent: "m_parents", Policy: Cascade},
		{Child: "a_children", Column: "z_id", Parent: "z_parents", Policy: Restrict},
		{Child: "b_children", Column: "z_id", Parent: "z_parents", Policy: Cascade},
	}
	if got := lockOrder(edges); !slices.Equal(got, want) {
		t.Fatalf("lockOrder = %v, want %v", got, want)
	}
	// The input is not reordered: Edges() hands out the catalog's own
	// order and Delete's cascade walk depends on it.
	if edges[0].Child != "b_children" || edges[0].Parent != "z_parents" {
		t.Errorf("lockOrder sorted its argument in place: %v", edges)
	}
}

// Every edge to one parent is consecutive in the order, so one table's
// locks are taken in one run of statements.
func TestLockOrderKeepsOneParentTogether(t *testing.T) {
	t.Parallel()

	order := lockOrder([]Edge{
		{Child: "a", Column: "p_id", Parent: "p", Policy: Cascade},
		{Child: "b", Column: "q_id", Parent: "q", Policy: Cascade},
		{Child: "c", Column: "p_id", Parent: "p", Policy: Restrict},
	})
	last := map[string]int{}
	for i, e := range order {
		if at, ok := last[e.Parent]; ok && at != i-1 {
			t.Errorf("edges to %s are not consecutive: %v", e.Parent, order)
		}
		last[e.Parent] = i
	}
}

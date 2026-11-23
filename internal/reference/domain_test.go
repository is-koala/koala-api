package reference_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/is-koala/koala-api/api"
	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/reference"
)

// TestProgramDegree binds every EducationDegree of the contract, read from
// the spec, to the program degree it names (ADR-0038): an associate degree
// names an associate degree program, every other degree a bachelor's
// program. A value added to the enum is red here until it is decided in
// ProgramDegree and in this table; a degree outside the contract names no
// program. The program degrees are the ones the reference files and
// departments.degree hold, so a renamed constant on either side is red
// too.
func TestProgramDegree(t *testing.T) {
	t.Parallel()

	if reference.ProgramAssociate != referencedata.DegreeAssociate || reference.ProgramBachelor != referencedata.DegreeBachelor {
		t.Fatalf("program degrees %s/%s, the files' %s/%s", reference.ProgramAssociate, reference.ProgramBachelor,
			referencedata.DegreeAssociate, referencedata.DegreeBachelor)
	}
	spec, err := api.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	schema, ok := spec.Components.Schemas["EducationDegree"]
	if !ok || schema.Value == nil {
		t.Fatal("the contract has no EducationDegree")
	}
	var enum []string
	for _, v := range schema.Value.Enum {
		enum = append(enum, fmt.Sprint(v))
	}
	want := map[string]string{
		"ASSOCIATE": referencedata.DegreeAssociate,
		"BACHELOR":  referencedata.DegreeBachelor,
		"MASTER":    referencedata.DegreeBachelor,
		"DOCTORATE": referencedata.DegreeBachelor,
	}
	if got := slices.Sorted(slices.Values(enum)); !slices.Equal(got, slices.Sorted(maps.Keys(want))) {
		t.Errorf("EducationDegree is %v; decide every value in ProgramDegree and here (%v)", got, slices.Sorted(maps.Keys(want)))
	}
	for _, degree := range enum {
		if got := reference.ProgramDegree(degree); got == "" || got != want[degree] {
			t.Errorf("ProgramDegree(%s) = %q, want %q", degree, got, want[degree])
		}
	}
	for _, degree := range []string{"", "HIGH_SCHOOL", "associate", "PHD"} {
		if got := reference.ProgramDegree(degree); got != "" {
			t.Errorf("ProgramDegree(%q) = %q, want none", degree, got)
		}
	}
}

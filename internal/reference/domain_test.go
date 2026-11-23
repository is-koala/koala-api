package reference_test

import (
	"testing"

	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/apiv1"
	"github.com/is-koala/koala-api/internal/reference"
)

// TestProgramDegree binds every EducationDegree of the contract to the
// program degree it names (ADR-0038): an associate degree names an
// associate degree program, every other degree a bachelor's program. The
// program degrees are the ones the reference files and departments.degree
// hold, so a renamed constant on either side is red here.
func TestProgramDegree(t *testing.T) {
	t.Parallel()

	if reference.ProgramAssociate != referencedata.DegreeAssociate || reference.ProgramBachelor != referencedata.DegreeBachelor {
		t.Fatalf("program degrees %s/%s, the files' %s/%s", reference.ProgramAssociate, reference.ProgramBachelor,
			referencedata.DegreeAssociate, referencedata.DegreeBachelor)
	}
	want := map[apiv1.EducationDegree]string{
		apiv1.EducationDegreeASSOCIATE: referencedata.DegreeAssociate,
		apiv1.EducationDegreeBACHELOR:  referencedata.DegreeBachelor,
		apiv1.EducationDegreeMASTER:    referencedata.DegreeBachelor,
		apiv1.EducationDegreeDOCTORATE: referencedata.DegreeBachelor,
	}
	for degree, program := range want {
		if !degree.Valid() {
			t.Errorf("%s is not an EducationDegree of the contract", degree)
		}
		if got := reference.ProgramDegree(string(degree)); got != program {
			t.Errorf("ProgramDegree(%s) = %s, want %s", degree, got, program)
		}
	}
}

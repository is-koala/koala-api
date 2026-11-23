package dbtest

import (
	"maps"
	"slices"
	"testing"
	"testing/fstest"
)

// TestTemplateNameCoversTheReferenceData: the template is rebuilt when a
// reference file changes (ADR-0037). If the hash left the files out, a
// test run after a yearly update would clone last year's rows and the
// count tests would read a stale template as the new data.
func TestTemplateNameCoversTheReferenceData(t *testing.T) {
	t.Parallel()

	ref := referenceFS()
	want := []string{
		"reference/cities.csv", "reference/countries.csv", "reference/departments.csv",
		"reference/districts.csv", "reference/faculties.csv", "reference/universities.csv",
	}
	if got := slices.Sorted(maps.Keys(ref)); !slices.Equal(got, want) {
		t.Fatalf("referenceFS() = %v, want %v", got, want)
	}
	migrations := fstest.MapFS{"0001_x.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}
	base := templateName(migrations, ref)

	changed := maps.Clone(ref)
	data := slices.Clone(changed["reference/universities.csv"].Data)
	data[len(data)-2] ^= 1
	changed["reference/universities.csv"] = &fstest.MapFile{Data: data}
	if templateName(migrations, changed) == base {
		t.Error("a changed reference file keeps the template name")
	}
	if templateName(migrations, fstest.MapFS{}) == base {
		t.Error("the template name does not depend on the reference files")
	}
}

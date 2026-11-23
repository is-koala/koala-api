package dbtest

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// TestTemplateNameCoversTheReferenceData: the template New clones is named
// by the migrations, the reference files, VERSION and the sync's own Go
// files (ADR-0037). If any of them were left out, a test run after a
// change would clone a template built from the old data or the old sync
// and the count tests would read it as the new one.
func TestTemplateNameCoversTheReferenceData(t *testing.T) {
	t.Parallel()

	ref := referenceFS()
	names := slices.Sorted(maps.Keys(ref))
	for _, want := range []string{
		"reference/VERSION", "reference/cities.csv", "reference/countries.csv", "reference/department_source_codes.csv", "reference/departments.csv",
		"reference/districts.csv", "reference/faculties.csv", "reference/universities.csv",
		"reference/src/referencedata.go", "reference/src/sync.go",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("referenceFS() lacks %s: %v", want, names)
		}
	}

	// New's own name: the one templateOf gives, over the files above.
	fsys, name := templateOf(t)
	if name != templateName(fsys, ref) {
		t.Fatalf("templateOf names %s, the migrations and reference files name %s", name, templateName(fsys, ref))
	}
	if name == templateName(fsys, fstest.MapFS{}) {
		t.Fatal("New's template name does not depend on the reference files")
	}
	for _, file := range []string{"reference/universities.csv", "reference/VERSION", "reference/src/sync.go"} {
		changed := maps.Clone(ref)
		changed[file] = &fstest.MapFile{Data: []byte(strings.ToUpper(string(ref[file].Data)) + "x")}
		if templateName(fsys, changed) == name {
			t.Errorf("a changed %s keeps the template name", file)
		}
	}
}

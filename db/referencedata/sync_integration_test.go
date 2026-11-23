//go:build integration

package referencedata_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/is-koala/koala-api/db/referencedata"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/dbtest"
)

// load returns a fresh copy of the embedded data, so a test may change it.
func load(t *testing.T) *referencedata.Dataset {
	t.Helper()
	d, err := referencedata.Load()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// runSync runs Sync in its own owner transaction and commits it. It also
// reports whether the transaction was given an id, which a transaction
// gets with its first write: a sync that wrote nothing has none.
func runSync(t *testing.T, owner *db.DB, d *referencedata.Dataset) (referencedata.Summary, bool, error) {
	t.Helper()
	var (
		s     referencedata.Summary
		wrote bool
	)
	err := owner.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		var err error
		if s, err = referencedata.Sync(ctx, q, d); err != nil {
			return err
		}
		return q.QueryRow(ctx, "SELECT pg_current_xact_id_if_assigned() IS NOT NULL").Scan(&wrote)
	})
	return s, wrote, err
}

// counts reads, per table, the rows in use and the retired ones.
func counts(t *testing.T, owner *db.DB) (active, retired map[string]int) {
	t.Helper()
	active, retired = map[string]int{}, map[string]int{}
	for _, table := range referencedata.Tables() {
		var a, r int
		err := owner.Pool().QueryRow(t.Context(),
			"SELECT count(*) FILTER (WHERE retired_at IS NULL), count(*) FILTER (WHERE retired_at IS NOT NULL) FROM "+table).
			Scan(&a, &r)
		if err != nil {
			t.Fatal(err)
		}
		active[table], retired[table] = a, r
	}
	return active, retired
}

// TestTemplateHoldsTheReferenceData: every test database comes with the
// reference rows, as many as the files list (README.md). A template built
// without the sync, or a sync that loads part of a file, is red here.
func TestTemplateHoldsTheReferenceData(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	active, retired := counts(t, owner)
	want := map[string]int{
		"countries": 1, "cities": 81, "districts": 995,
		"universities": 223, "faculties": 2326, "departments": 11250,
	}
	for table, n := range want {
		if active[table] != n || retired[table] != 0 {
			t.Errorf("%s: %d in use, %d retired; want %d and 0", table, active[table], retired[table], n)
		}
	}
	var name string
	err := owner.Pool().QueryRow(t.Context(),
		"SELECT name FROM departments WHERE id = '97d43fdf-0a52-4f95-9dab-8d5adbf2d37f' AND faculty_id = 'a8004de5-6f16-41ea-9841-42ed646684cf'").
		Scan(&name)
	if err != nil || name != "Psikoloji (İngilizce)" {
		t.Errorf("a known department: %q, %v", name, err)
	}
}

// TestSyncIsIdempotent: a second run over the same files writes nothing,
// not even a row rewritten to the same value (the transaction gets no id).
func TestSyncIsIdempotent(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	d := load(t)
	s, wrote, err := runSync(t, owner, d)
	if err != nil {
		t.Fatal(err)
	}
	if wrote || s.Changed() {
		t.Fatalf("second sync wrote: %+v", s)
	}
	for _, c := range s {
		if c.Unchanged == 0 {
			t.Errorf("%s: nothing unchanged: %+v", c.Table, c)
		}
	}
}

// TestSyncFollowsTheFiles drives one database through a year of changes:
// a dropped department and a dropped faculty (with its departments) are
// retired, not deleted; a renamed university is updated under its id; a
// new department is inserted; and when the dropped rows come back they
// are in use again under the same ids.
func TestSyncFollowsTheFiles(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	d := load(t)
	full := load(t)

	dropDept := d.Departments[0]
	dropFaculty := d.Faculties[len(d.Faculties)-1]
	var dropped []uuid.UUID // departments of dropFaculty
	kept := d.Departments[:0:0]
	for _, r := range d.Departments[1:] {
		if r.Parent == dropFaculty.ID {
			dropped = append(dropped, r.ID)
			continue
		}
		kept = append(kept, r)
	}
	d.Departments = kept
	d.Faculties = d.Faculties[:len(d.Faculties)-1]
	d.Universities[0].Name = "YENİ ADLI ÜNİVERSİTE"
	added := referencedata.Row{ID: uuid.New(), Parent: d.Faculties[0].ID, Name: "Yeni Bölüm"}
	d.Departments = append(d.Departments, added)

	s, _, err := runSync(t, owner, d)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]referencedata.Counts{
		"universities": {Updated: 1, Unchanged: 222},
		"faculties":    {Retired: 1, Unchanged: 2325},
		"departments":  {Inserted: 1, Retired: int64(1 + len(dropped)), Unchanged: int64(len(d.Departments) - 1)},
		"countries":    {Unchanged: 1},
		"cities":       {Unchanged: 81},
		"districts":    {Unchanged: 995},
	}
	for _, c := range s {
		if c.Counts != want[c.Table] {
			t.Errorf("%s: %+v, want %+v", c.Table, c.Counts, want[c.Table])
		}
	}
	var retired int
	err = owner.Pool().QueryRow(t.Context(),
		"SELECT count(*) FROM departments WHERE id = ANY($1::uuid[]) AND retired_at IS NOT NULL",
		append([]uuid.UUID{dropDept.ID}, dropped...)).Scan(&retired)
	if err != nil || retired != 1+len(dropped) {
		t.Errorf("dropped departments: %d retired rows (%v), want %d kept and retired", retired, err, 1+len(dropped))
	}

	// The next year lists them again.
	s, _, err = runSync(t, owner, full)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s {
		switch c.Table {
		case "universities":
			if c.Updated != 1 {
				t.Errorf("universities: %+v, want the name back", c.Counts)
			}
		case "faculties":
			if c.Updated != 1 || c.Retired != 0 {
				t.Errorf("faculties: %+v, want the faculty in use again", c.Counts)
			}
		case "departments":
			if c.Updated != int64(1+len(dropped)) || c.Retired != 1 || c.Inserted != 0 {
				t.Errorf("departments: %+v, want %d back, the added one retired", c.Counts, 1+len(dropped))
			}
		}
	}
	active, retiredBy := counts(t, owner)
	if active["departments"] != 11250 || retiredBy["departments"] != 1 {
		t.Errorf("departments: %d in use and %d retired, want 11250 and 1", active["departments"], retiredBy["departments"])
	}
}

// TestSyncRefusesAMovedRow: an id the database has under one faculty and
// the files give under another is refused, and nothing is written.
func TestSyncRefusesAMovedRow(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	d := load(t)
	moved := d.Departments[0]
	for _, f := range d.Faculties {
		if f.ID != moved.Parent {
			d.Departments[0].Parent = f.ID
			break
		}
	}
	d.Universities[0].Name = "BU YAZILMAMALI"
	_, _, err := runSync(t, owner, d)
	if err == nil || !strings.Contains(err.Error(), moved.ID.String()) || !strings.Contains(err.Error(), "new id") {
		t.Fatalf("sync = %v, want the moved department refused", err)
	}
	var parent uuid.UUID
	if err := owner.Pool().QueryRow(t.Context(), "SELECT faculty_id FROM departments WHERE id = $1", moved.ID).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	var renamed bool
	if err := owner.Pool().QueryRow(t.Context(), "SELECT EXISTS (SELECT FROM universities WHERE name = 'BU YAZILMAMALI')").Scan(&renamed); err != nil {
		t.Fatal(err)
	}
	if parent != moved.Parent || renamed {
		t.Errorf("after the refusal: parent %s (want %s), renamed %v; the transaction should have written nothing", parent, moved.Parent, renamed)
	}
}

// TestSyncRefusesInvalidData: Sync validates the data itself, so a caller
// that changed a Dataset after Load cannot write what Load would refuse.
func TestSyncRefusesInvalidData(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	d := load(t)
	d.Faculties = nil // every department is an orphan
	d.Universities = d.Universities[1:]
	_, _, err := runSync(t, owner, d)
	if err == nil || !strings.Contains(err.Error(), "is not a row of") {
		t.Fatalf("sync = %v, want the orphans refused", err)
	}
	active, retired := counts(t, owner)
	if active["universities"] != 223 || retired["universities"] != 0 {
		t.Errorf("universities: %d in use, %d retired after a refused sync; want 223 and 0", active["universities"], retired["universities"])
	}
}

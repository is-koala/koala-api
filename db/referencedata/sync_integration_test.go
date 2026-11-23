//go:build integration

package referencedata_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
func runSync(t *testing.T, owner *db.DB, d *referencedata.Dataset, opts referencedata.Options) (referencedata.Result, bool, error) {
	t.Helper()
	var (
		res   referencedata.Result
		wrote bool
	)
	err := owner.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		var err error
		if res, err = referencedata.Sync(ctx, q, d, opts); err != nil {
			return err
		}
		return q.QueryRow(ctx, "SELECT pg_current_xact_id_if_assigned() IS NOT NULL").Scan(&wrote)
	})
	return res, wrote, err
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

func version(t *testing.T, owner *db.DB) int {
	t.Helper()
	var v int
	if err := owner.Pool().QueryRow(t.Context(), "SELECT coalesce((SELECT version FROM reference_data_version), 0)").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func of(res referencedata.Result, table string) referencedata.TableResult {
	for _, r := range res.Tables {
		if r.Table == table {
			return r
		}
	}
	return referencedata.TableResult{}
}

// TestTemplateHoldsTheReferenceData: every test database comes with the
// reference rows, as many as the files list (README.md), and the data
// version. A template built without the sync, or a sync that loads part
// of a file, is red here.
func TestTemplateHoldsTheReferenceData(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	active, retired := counts(t, owner)
	want := map[string]int{
		"countries": 1, "cities": 81, "districts": 972,
		"universities": 224, "faculties": 2047, "departments": 7571,
	}
	for table, n := range want {
		if active[table] != n || retired[table] != 0 {
			t.Errorf("%s: %d in use, %d retired; want %d and 0", table, active[table], retired[table], n)
		}
	}
	if v := version(t, owner); v != 1 {
		t.Errorf("data version %d, want 1", v)
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
// not even a row rewritten to the same value or the version (the
// transaction gets no id).
func TestSyncIsIdempotent(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	res, wrote, err := runSync(t, owner, load(t), referencedata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if wrote || res.Changed() || res.Skipped {
		t.Fatalf("second sync wrote: %+v", res)
	}
	for _, c := range res.Tables {
		if c.Unchanged == 0 || c.Unchanged != c.Active {
			t.Errorf("%s: %d unchanged of %d in use", c.Table, c.Unchanged, c.Active)
		}
	}
}

// TestSyncFollowsTheFiles drives one database through a year of changes:
// a dropped department and a dropped faculty (with its departments) are
// retired, not deleted; a renamed university is updated under its id; a
// new department is inserted; and when the dropped rows come back they
// are in use again under the same ids. The result names every row.
func TestSyncFollowsTheFiles(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	d := load(t)
	full := load(t)

	dropDept := d.Departments[0]
	dropFaculty := d.Faculties[len(d.Faculties)-1]
	dropped := map[uuid.UUID]bool{dropDept.ID: true}
	kept := d.Departments[:0:0]
	for _, r := range d.Departments {
		if r.Parent == dropFaculty.ID || r.ID == dropDept.ID {
			dropped[r.ID] = true
			continue
		}
		kept = append(kept, r)
	}
	d.Departments = kept
	d.Faculties = d.Faculties[:len(d.Faculties)-1]
	oldName := d.Universities[0].Name
	d.Universities[0].Name = "YENİ ADLI ÜNİVERSİTE"
	added := referencedata.Row{ID: uuid.New(), Parent: d.Faculties[0].ID, Name: "Yeni Bölüm"}
	d.Departments = append(d.Departments, added)

	res, _, err := runSync(t, owner, d, referencedata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	u, f, dep := of(res, "universities"), of(res, "faculties"), of(res, "departments")
	if len(u.Updated) != 1 || u.Updated[0].ID != d.Universities[0].ID || u.Updated[0].OldName != oldName ||
		u.Updated[0].Name != "YENİ ADLI ÜNİVERSİTE" || u.Updated[0].Restored {
		t.Errorf("universities: %+v, want the rename with both names", u.Updated)
	}
	if len(f.Retired) != 1 || f.Retired[0].ID != dropFaculty.ID || f.Retired[0].Name != dropFaculty.Name {
		t.Errorf("faculties: retired %+v, want %s", f.Retired, dropFaculty.ID)
	}
	if len(dep.Inserted) != 1 || dep.Inserted[0].ID != added.ID || len(dep.Retired) != len(dropped) {
		t.Errorf("departments: inserted %+v, %d retired; want %s and %d", dep.Inserted, len(dep.Retired), added.ID, len(dropped))
	}
	for _, c := range dep.Retired {
		if !dropped[c.ID] || c.Name == "" {
			t.Errorf("departments: retired %+v, not a dropped row with its name", c)
		}
	}
	active, retired := counts(t, owner)
	if retired["departments"] != len(dropped) || active["departments"] != 7571-len(dropped)+1 {
		t.Errorf("departments: %d in use, %d retired", active["departments"], retired["departments"])
	}

	// The next year lists them again.
	res, _, err = runSync(t, owner, full, referencedata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	u, f, dep = of(res, "universities"), of(res, "faculties"), of(res, "departments")
	if len(u.Updated) != 1 || u.Updated[0].OldName != "YENİ ADLI ÜNİVERSİTE" || u.Updated[0].Name != oldName {
		t.Errorf("universities: %+v, want the name back", u.Updated)
	}
	if len(f.Updated) != 1 || !f.Updated[0].Restored || len(f.Retired) != 0 {
		t.Errorf("faculties: %+v, want the faculty in use again", f)
	}
	if len(dep.Updated) != len(dropped) || len(dep.Retired) != 1 || dep.Retired[0].ID != added.ID || len(dep.Inserted) != 0 {
		t.Errorf("departments: %d back, retired %+v; want %d back and the added one retired", len(dep.Updated), dep.Retired, len(dropped))
	}
	for _, c := range dep.Updated {
		if !c.Restored || !dropped[c.ID] {
			t.Errorf("departments: updated %+v, want a dropped row brought back", c)
		}
	}
	active, retired = counts(t, owner)
	if active["departments"] != 7571 || retired["departments"] != 1 {
		t.Errorf("departments: %d in use and %d retired, want 7571 and 1", active["departments"], retired["departments"])
	}
}

// TestSyncStopsAtTheRetireLimit: a file cut short, here a departments.csv
// of its header alone, would retire every department; the sync stops
// before writing anything and names the table. An explicit allowance of
// that many rows lets it through; the limit itself is exact.
func TestSyncStopsAtTheRetireLimit(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	d := load(t)
	d.Departments = nil

	_, _, err := runSync(t, owner, d, referencedata.Options{})
	var limit *referencedata.RetireLimitError
	if !errors.As(err, &limit) || limit.Table != "departments" || limit.Retire != 7571 || limit.Limit != 378 {
		t.Fatalf("sync = %v, want the departments' retirement limit", err)
	}
	if active, _ := counts(t, owner); active["departments"] != 7571 {
		t.Fatalf("after the refusal %d departments in use, want 7571", active["departments"])
	}
	_, _, err = runSync(t, owner, d, referencedata.Options{AllowRetire: map[string]int{"departments": 7570}})
	if !errors.As(err, &limit) {
		t.Fatalf("an allowance one short: %v, want the limit", err)
	}

	// At the limit: 378 of 7571 pass, 379 do not.
	at := load(t)
	at.Departments = at.Departments[378:]
	over := load(t)
	over.Departments = over.Departments[379:]
	if _, _, err := runSync(t, owner, over, referencedata.Options{}); !errors.As(err, &limit) || limit.Retire != 379 {
		t.Fatalf("379 retirements: %v, want the limit", err)
	}
	res, _, err := runSync(t, owner, at, referencedata.Options{})
	if err != nil || len(of(res, "departments").Retired) != 378 {
		t.Fatalf("378 retirements: %v, %+v", err, of(res, "departments"))
	}

	// The whole table, allowed.
	fresh := dbtest.New(t).Open(t, db.RoleOwner)
	res, _, err = runSync(t, fresh, d, referencedata.Options{AllowRetire: map[string]int{"departments": 7571}})
	if err != nil || len(of(res, "departments").Retired) != 7571 {
		t.Fatalf("allowed: %v, %d retired", err, len(of(res, "departments").Retired))
	}
}

// TestSyncKeepsANewerVersion: a database that holds a newer data version
// than the files (a release rolled back to an older image) is left as it
// is: the sync writes nothing and says so, without an error. The same
// version syncs, and a newer one syncs and is recorded.
func TestSyncKeepsANewerVersion(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	newer := load(t)
	newer.Version = 2
	newer.Universities[0].Name = "SÜRÜM 2 ADI"
	res, _, err := runSync(t, owner, newer, referencedata.Options{})
	if err != nil || res.Skipped || res.DatabaseVersion != 1 || len(of(res, "universities").Updated) != 1 {
		t.Fatalf("version 2 over 1: %+v, %v", res, err)
	}
	if v := version(t, owner); v != 2 {
		t.Fatalf("recorded version %d, want 2", v)
	}

	older := load(t) // version 1, the old name
	res, wrote, err := runSync(t, owner, older, referencedata.Options{})
	if err != nil || !res.Skipped || res.DataVersion != 1 || res.DatabaseVersion != 2 || wrote || res.Changed() {
		t.Fatalf("version 1 over 2: %+v (wrote %v), %v; want it skipped", res, wrote, err)
	}
	var name string
	if err := owner.Pool().QueryRow(t.Context(), "SELECT name FROM universities WHERE id = $1", newer.Universities[0].ID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "SÜRÜM 2 ADI" || version(t, owner) != 2 {
		t.Fatalf("after the older sync: %q, version %d; want version 2's data kept", name, version(t, owner))
	}

	// The same version repairs a drift and leaves the version as it is.
	if _, err := owner.Pool().Exec(t.Context(), "UPDATE universities SET name = 'EL İLE' WHERE id = $1", newer.Universities[0].ID); err != nil {
		t.Fatal(err)
	}
	res, _, err = runSync(t, owner, newer, referencedata.Options{})
	if err != nil || res.Skipped || len(of(res, "universities").Updated) != 1 || version(t, owner) != 2 {
		t.Fatalf("version 2 over 2: %+v, %v", res, err)
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
	_, wrote, err := runSync(t, owner, d, referencedata.Options{})
	if err == nil || !strings.Contains(err.Error(), moved.ID.String()) || !strings.Contains(err.Error(), "new id") || wrote {
		t.Fatalf("sync = %v (wrote %v), want the moved department refused before any write", err, wrote)
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
	_, _, err := runSync(t, owner, d, referencedata.Options{})
	if err == nil || !strings.Contains(err.Error(), "is not a row of") {
		t.Fatalf("sync = %v, want the orphans refused", err)
	}
	if active, _ := counts(t, owner); active["universities"] != 224 {
		t.Errorf("universities: %d in use after a refused sync; want 224", active["universities"])
	}
	_, _, err = runSync(t, owner, load(t), referencedata.Options{AllowRetire: map[string]int{"users": 1}})
	if err == nil || !strings.Contains(err.Error(), "not a reference table") {
		t.Fatalf("an allowance for users: %v", err)
	}
}

// TestConcurrentSyncsSerialize: two syncs of one database take turns on
// the advisory lock. The second waits while the first holds it, then plans
// on what the first committed and has nothing to do. Without the lock the
// second would plan on the snapshot before the first's commit, insert the
// same row again and fail on the primary key.
func TestConcurrentSyncsSerialize(t *testing.T) {
	t.Parallel()

	// Three pools: an owner pool holds two connections, and the first
	// sync, the second and the watcher each need one at the same time.
	database := dbtest.New(t)
	owner, second, watcher := database.Open(t, db.RoleOwner), database.Open(t, db.RoleOwner), database.Open(t, db.RoleOwner)
	d := load(t)
	d.Version = 2
	d.Departments = append(d.Departments, referencedata.Row{ID: uuid.New(), Parent: d.Faculties[0].ID, Name: "Eşzamanlı Bölüm"})
	ctx := t.Context()

	first, err := owner.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(context.WithoutCancel(ctx)) }()
	res1, err := referencedata.Sync(ctx, first, d, referencedata.Options{})
	if err != nil || len(of(res1, "departments").Inserted) != 1 {
		t.Fatalf("first sync: %+v, %v", res1, err)
	}

	type outcome struct {
		res referencedata.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		var o outcome
		o.err = second.InTx(ctx, func(ctx context.Context, q db.Querier) error {
			var err error
			o.res, err = referencedata.Sync(ctx, q, d, referencedata.Options{})
			return err
		})
		done <- o
	}()

	// The second waits on the advisory lock, not on a row.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		err := watcher.Pool().QueryRow(ctx,
			"SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())").
			Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		select {
		case o := <-done:
			t.Fatalf("the second sync did not wait for the first: %+v, %v", o.res, o.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the second sync never waited on the advisory lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	o := <-done
	if o.err != nil || o.res.Changed() || o.res.DatabaseVersion != 2 {
		t.Fatalf("second sync after the first: %+v, %v; want nothing to do", o.res, o.err)
	}
}

// TestNoIndexUsesTheTurkishCollation: no index orders by the turkish
// collation today, so an ICU upgrade needs no REINDEX (runbook,
// veritabani.md). The day an index on a reference name lands (the address
// PR may add one), this is red and the runbook's REINDEX step applies.
func TestNoIndexUsesTheTurkishCollation(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	var indexes []string
	rows, err := owner.Pool().Query(t.Context(), `
		SELECT i.indexrelid::regclass::text
		FROM pg_index i JOIN pg_collation c ON c.oid = ANY (i.indcollation::oid[])
		WHERE c.collname = 'turkish'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 0 {
		t.Errorf("indexes %v use the turkish collation: an ICU upgrade must REINDEX them (docs/runbook/veritabani.md); update this test and the runbook together", indexes)
	}
	var collations int
	if err := owner.Pool().QueryRow(t.Context(), "SELECT count(*) FROM pg_collation WHERE collname = 'turkish' AND collprovider = 'i'").Scan(&collations); err != nil || collations != 1 {
		t.Fatalf("the turkish ICU collation: %d, %v", collations, err)
	}
}

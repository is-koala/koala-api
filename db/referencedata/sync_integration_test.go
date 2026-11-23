//go:build integration

package referencedata_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is-koala/koala-api/db/migrations"
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

// runSync runs Sync in its own owner transaction and commits it unless
// Sync fails. It also reports whether the transaction was given an id,
// which a transaction gets with its first write: a sync that wrote
// nothing has none. The id is read after a failed Sync too, before the
// rollback, so a refusal can be shown to come before any write.
func runSync(t *testing.T, owner *db.DB, d *referencedata.Dataset, opts referencedata.Options) (referencedata.Result, bool, error) {
	t.Helper()
	var (
		res     referencedata.Result
		wrote   bool
		syncErr error
	)
	err := owner.InTx(t.Context(), func(ctx context.Context, q db.Querier) error {
		res, syncErr = referencedata.Sync(ctx, q, d, opts)
		if err := q.QueryRow(ctx, "SELECT pg_current_xact_id_if_assigned() IS NOT NULL").Scan(&wrote); err != nil {
			return err
		}
		return syncErr
	})
	if syncErr != nil {
		return res, wrote, syncErr
	}
	return res, wrote, err
}

// dataVersion is the files' version (TestDatasetCounts binds it).
const dataVersion = 2

// allow is an allowance for the files' data version: rows per table.
func allow(rows map[string]int) referencedata.Options {
	opts := referencedata.Options{AllowRetire: map[string]referencedata.Allowance{}}
	for table, n := range rows {
		opts.AllowRetire[table] = referencedata.Allowance{Rows: n, Version: dataVersion}
	}
	return opts
}

// dropDepartments takes departments out of d, and their source codes with
// them, as the files do when a program closes.
func dropDepartments(d *referencedata.Dataset, drop func(referencedata.Row) bool) map[uuid.UUID]bool {
	dropped := map[uuid.UUID]bool{}
	kept := d.Departments[:0:0]
	for _, r := range d.Departments {
		if drop(r) {
			dropped[r.ID] = true
			continue
		}
		kept = append(kept, r)
	}
	d.Departments = kept
	codes := d.DepartmentSourceCodes[:0:0]
	for _, c := range d.DepartmentSourceCodes {
		if !dropped[c.Department] {
			codes = append(codes, c)
		}
	}
	d.DepartmentSourceCodes = codes
	return dropped
}

func revision(t *testing.T, owner *db.DB) int {
	t.Helper()
	var r int
	if err := owner.Pool().QueryRow(t.Context(), "SELECT coalesce((SELECT revision FROM reference_data_version), 0)").Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
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
		"countries": 1, "cities": 81, "districts": 973,
		"universities": 231, "faculties": 3022, "departments": 14711, "department_source_codes": 9254,
	}
	for table, n := range want {
		if active[table] != n || retired[table] != 0 {
			t.Errorf("%s: %d in use, %d retired; want %d and 0", table, active[table], retired[table], n)
		}
	}
	if v := version(t, owner); v != dataVersion {
		t.Errorf("data version %d, want %d", v, dataVersion)
	}
	// The degree reaches the table: the associate programs are ASSOCIATE
	// and Uçak Teknolojisi of Ankara Havacılık MYO has its two codes.
	var associate, codes int
	err := owner.Pool().QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM departments WHERE degree = 'ASSOCIATE'),
		       (SELECT count(*) FROM department_source_codes
		        WHERE department_id = '01a207cf-03cc-7a3c-9df7-92d5f5825c58' AND code IN ('205750094', '205750101'))`).
		Scan(&associate, &codes)
	if err != nil || associate != 7144 || codes != 2 {
		t.Errorf("associate programs %d (want 7144), Uçak Teknolojisi's codes %d (want 2): %v", associate, codes, err)
	}
	var name string
	err = owner.Pool().QueryRow(t.Context(),
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
	// The last faculty is a vocational school of version 2: its programs
	// carry source codes, which are retired with them and come back with
	// them.
	dropFaculty := d.Faculties[len(d.Faculties)-1]
	codesBefore := len(d.DepartmentSourceCodes)
	dropped := dropDepartments(d, func(r referencedata.Row) bool { return r.Parent == dropFaculty.ID || r.ID == dropDept.ID })
	droppedCodes := codesBefore - len(d.DepartmentSourceCodes)
	if droppedCodes == 0 {
		t.Fatalf("the last faculty %s %q has no program with a source code", dropFaculty.ID, dropFaculty.Name)
	}
	d.Faculties = d.Faculties[:len(d.Faculties)-1]
	oldName := d.Universities[0].Name
	d.Universities[0].Name = "YENİ ADLI ÜNİVERSİTE"
	added := referencedata.Row{ID: uuid.New(), Parent: d.Faculties[0].ID, Degree: referencedata.DegreeBachelor, Name: "Yeni Bölüm"}
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
	if c := of(res, "department_source_codes"); len(c.Retired) != droppedCodes || len(c.Inserted)+len(c.Updated) != 0 {
		t.Errorf("source codes: %d retired, want %d: %+v", len(c.Retired), droppedCodes, c)
	}
	active, retired := counts(t, owner)
	if retired["departments"] != len(dropped) || active["departments"] != 14711-len(dropped)+1 {
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
	if c := of(res, "department_source_codes"); len(c.Updated) != droppedCodes || len(c.Retired)+len(c.Inserted) != 0 {
		t.Errorf("source codes: %d back, want %d: %+v", len(c.Updated), droppedCodes, c)
	}
	for _, c := range of(res, "department_source_codes").Updated {
		if !c.Restored || !dropped[c.Parent] {
			t.Errorf("source codes: updated %+v, want a code of a dropped department brought back", c)
		}
	}
	active, retired = counts(t, owner)
	if active["departments"] != 14711 || retired["departments"] != 1 || active["department_source_codes"] != 9254 {
		t.Errorf("departments: %d in use and %d retired, want 14711 and 1; %d codes in use",
			active["departments"], retired["departments"], active["department_source_codes"])
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
	d.Departments, d.DepartmentSourceCodes = nil, nil

	_, wrote, err := runSync(t, owner, d, referencedata.Options{})
	var limit *referencedata.RetireLimitError
	if !errors.As(err, &limit) || limit.Table != "departments" || limit.Retire != 14711 || limit.Limit != 735 {
		t.Fatalf("sync = %v, want the departments' retirement limit", err)
	}
	// Plan, then limit, then write: the refused sync's transaction never
	// wrote a row, so it was never given an id.
	if wrote {
		t.Fatal("the refused sync wrote before the limit stopped it")
	}
	if active, _ := counts(t, owner); active["departments"] != 14711 {
		t.Fatalf("after the refusal %d departments in use, want 14711", active["departments"])
	}
	_, _, err = runSync(t, owner, d, allow(map[string]int{"departments": 14710, "department_source_codes": 9254}))
	if !errors.As(err, &limit) || limit.Table != "departments" {
		t.Fatalf("an allowance one short: %v, want the limit", err)
	}
	// The source codes have their own limit: the departments' allowance
	// does not lift it.
	_, wrote, err = runSync(t, owner, d, allow(map[string]int{"departments": 14711}))
	if !errors.As(err, &limit) || limit.Table != "department_source_codes" || limit.Retire != 9254 || limit.Limit != 462 || wrote {
		t.Fatalf("the departments allowed, not their codes: %v (wrote %v), want the codes' limit", err, wrote)
	}

	// At the limit: 735 of 14711 pass, 736 do not. The first 7567 rows are
	// the bachelor's programs, without codes.
	at := load(t)
	at.Departments = at.Departments[735:]
	over := load(t)
	over.Departments = over.Departments[736:]
	if _, _, err := runSync(t, owner, over, referencedata.Options{}); !errors.As(err, &limit) || limit.Retire != 736 {
		t.Fatalf("736 retirements: %v, want the limit", err)
	}
	res, _, err := runSync(t, owner, at, referencedata.Options{})
	if err != nil || len(of(res, "departments").Retired) != 735 {
		t.Fatalf("735 retirements: %v, %+v", err, of(res, "departments"))
	}

	// The whole table, allowed.
	fresh := dbtest.New(t).Open(t, db.RoleOwner)
	res, _, err = runSync(t, fresh, d, allow(map[string]int{"departments": 14711, "department_source_codes": 9254}))
	if err != nil || len(of(res, "departments").Retired) != 14711 || len(of(res, "department_source_codes").Retired) != 9254 {
		t.Fatalf("allowed: %v, %d retired", err, len(of(res, "departments").Retired))
	}
}

// TestSourceCodesHoldTheRetireLimit: the source codes are a reference
// table like the others (ADR-0038). Codes dropped while their departments
// stay (a cut department_source_codes.csv) are retired under the same
// limit, 5 % of the pairs in use, and nothing is written above it; retired
// pairs from earlier years do not count (5,000 of them here). Within the
// limit the codes are retired, and listed again they are back in use.
func TestSourceCodesHoldTheRetireLimit(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	if _, err := owner.Pool().Exec(t.Context(), `
		INSERT INTO department_source_codes (code, department_id, retired_at)
		SELECT '9' || lpad(n::text, 8, '0'), '01a207cf-03cc-7a3c-9df7-92d5f5825c58', now()
		FROM generate_series(1, 5000) AS n`); err != nil {
		t.Fatal(err)
	}
	over := load(t)
	over.DepartmentSourceCodes = over.DepartmentSourceCodes[463:]
	_, wrote, err := runSync(t, owner, over, referencedata.Options{})
	var limit *referencedata.RetireLimitError
	if !errors.As(err, &limit) || limit.Table != "department_source_codes" || limit.Retire != 463 || limit.Limit != 462 ||
		limit.Active != 9254 || wrote {
		t.Fatalf("463 codes dropped next to 5,000 retired pairs: %v (wrote %v), want the limit of 462 over 9,254 pairs in use", err, wrote)
	}
	at := load(t)
	at.DepartmentSourceCodes = at.DepartmentSourceCodes[462:]
	res, _, err := runSync(t, owner, at, referencedata.Options{})
	if c := of(res, "department_source_codes"); err != nil || len(c.Retired) != 462 || c.Retired[0].Code == "" || c.Retired[0].ID != uuid.Nil {
		t.Fatalf("462 codes dropped: %v, %+v", err, c)
	}
	if active, retired := counts(t, owner); active["department_source_codes"] != 9254-462 || retired["department_source_codes"] != 5462 {
		t.Fatalf("codes: %d in use, %d retired", active["department_source_codes"], retired["department_source_codes"])
	}
	res, _, err = runSync(t, owner, load(t), referencedata.Options{})
	if c := of(res, "department_source_codes"); err != nil || len(c.Updated) != 462 || !c.Updated[0].Restored {
		t.Fatalf("the codes listed again: %v, %+v", err, c)
	}
}

// codePairs reads the (department, retired) pairs of a code.
func codePairs(t *testing.T, owner *db.DB, code string) map[uuid.UUID]bool {
	t.Helper()
	rows, err := owner.Pool().Query(t.Context(), "SELECT department_id, retired_at IS NOT NULL FROM department_source_codes WHERE code = $1", code)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uuid.UUID]bool{}
	var dept uuid.UUID
	var retired bool
	if _, err := pgx.ForEachRow(rows, []any{&dept, &retired}, func() error { out[dept] = retired; return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSyncReassignsACode: a row of department_source_codes is a (code,
// department) pair that never changes (ADR-0038). A code the files give to
// another department, as tools/id_decisions.csv decides when ÖSYM reuses
// a code, is the old pair retired and the new one inserted, in one sync
// and both in the result; the old program keeps its id. At most one pair
// per code is in use, in the database too. A code dropped and later given
// to a third department is a new pair beside the retired ones; listed
// again under its first department, that pair comes back and the other
// retires.
func TestSyncReassignsACode(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	d := load(t)
	code, first := d.DepartmentSourceCodes[0].Code, d.DepartmentSourceCodes[0].Department
	var others []uuid.UUID
	for _, c := range d.DepartmentSourceCodes {
		if c.Department != first && !slices.Contains(others, c.Department) {
			others = append(others, c.Department)
		}
		if len(others) == 2 {
			break
		}
	}
	second, third := others[0], others[1]
	with := func(department uuid.UUID) *referencedata.Dataset {
		d := load(t)
		if department == uuid.Nil {
			d.DepartmentSourceCodes = d.DepartmentSourceCodes[1:]
		} else {
			d.DepartmentSourceCodes[0].Department = department
		}
		return d
	}
	pairs := func(want map[uuid.UUID]bool) {
		t.Helper()
		if got := codePairs(t, owner, code); !maps.Equal(got, want) {
			t.Fatalf("code %s: pairs %v (department: retired), want %v", code, got, want)
		}
	}

	res, _, err := runSync(t, owner, with(second), referencedata.Options{})
	c := of(res, "department_source_codes")
	if err != nil || len(c.Retired) != 1 || c.Retired[0].Code != code || c.Retired[0].Parent != first ||
		len(c.Inserted) != 1 || c.Inserted[0].Code != code || c.Inserted[0].Parent != second || len(c.Updated) != 0 {
		t.Fatalf("the code given to another department: %v, %+v", err, c)
	}
	if dep := of(res, "departments"); len(dep.Retired)+len(dep.Updated)+len(dep.Inserted) != 0 {
		t.Errorf("departments: %+v, want them as they are", dep)
	}
	pairs(map[uuid.UUID]bool{first: true, second: false})

	// The database holds one pair in use per code.
	_, err = owner.Pool().Exec(t.Context(), "UPDATE department_source_codes SET retired_at = NULL WHERE code = $1 AND department_id = $2", code, first)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("a second pair in use: %v, want a unique violation", err)
	}

	// Dropped, then given to a third department: a new pair.
	if _, _, err := runSync(t, owner, with(uuid.Nil), referencedata.Options{}); err != nil {
		t.Fatal(err)
	}
	pairs(map[uuid.UUID]bool{first: true, second: true})
	res, _, err = runSync(t, owner, with(third), referencedata.Options{})
	if c := of(res, "department_source_codes"); err != nil || len(c.Inserted) != 1 || c.Inserted[0].Parent != third || len(c.Retired)+len(c.Updated) != 0 {
		t.Fatalf("a retired code given to a third department: %v, %+v", err, c)
	}
	pairs(map[uuid.UUID]bool{first: true, second: true, third: false})

	// Under its first department again: that pair is back, the third's
	// retires, in one sync.
	res, _, err = runSync(t, owner, load(t), referencedata.Options{})
	if c := of(res, "department_source_codes"); err != nil || len(c.Updated) != 1 || c.Updated[0].Parent != first || !c.Updated[0].Restored ||
		len(c.Retired) != 1 || c.Retired[0].Parent != third || len(c.Inserted) != 0 {
		t.Fatalf("the code back under its first department: %v, %+v", err, c)
	}
	pairs(map[uuid.UUID]bool{first: false, second: true, third: true})
}

// TestSyncRefusesADegreeChange: a department whose degree the files
// change is refused, and nothing is written: an associate degree program
// is not a bachelor's program, and records that name it hold a degree
// (ADR-0038). A retired department the files bring back with another
// degree is refused too: the degree check comes before the restore, so a
// row cannot change its degree by leaving and coming back.
func TestSyncRefusesADegreeChange(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	other := map[string]string{
		referencedata.DegreeBachelor: referencedata.DegreeAssociate, referencedata.DegreeAssociate: referencedata.DegreeBachelor,
	}
	for _, retired := range []bool{false, true} {
		for _, from := range []string{referencedata.DegreeBachelor, referencedata.DegreeAssociate} {
			d := load(t)
			i := slices.IndexFunc(d.Departments, func(r referencedata.Row) bool { return r.Degree == from })
			if retired {
				i = slices.IndexFunc(d.Departments[i+1:], func(r referencedata.Row) bool { return r.Degree == from }) + i + 1
			}
			changed := d.Departments[i]
			if retired {
				if _, err := owner.Pool().Exec(t.Context(), "UPDATE departments SET retired_at = now() WHERE id = $1", changed.ID); err != nil {
					t.Fatal(err)
				}
			}
			d.Departments[i].Degree = other[from]
			d.Universities[0].Name = "BU YAZILMAMALI"
			_, wrote, err := runSync(t, owner, d, referencedata.Options{})
			if err == nil || !strings.Contains(err.Error(), changed.ID.String()) || !strings.Contains(err.Error(), "another degree") || wrote {
				t.Fatalf("%s %s (retired %v) changed: sync = %v (wrote %v), want it refused before any write", from, changed.ID, retired, err, wrote)
			}
			var stillRetired bool
			var degree string
			if err := owner.Pool().QueryRow(t.Context(), "SELECT retired_at IS NOT NULL, degree FROM departments WHERE id = $1", changed.ID).
				Scan(&stillRetired, &degree); err != nil || stillRetired != retired || degree != from {
				t.Fatalf("after the refusal: retired %v, degree %s (%v); want %v and %s", stillRetired, degree, err, retired, from)
			}
		}
	}
}

// mintedIDs reads the ids tools/from_yok.py minted, from its ledger: the
// universities, faculties and departments version 2 added to the Java
// snapshot (ADR-0038). The test runs in this package's directory.
func mintedIDs(t *testing.T) map[uuid.UUID]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("tools", "id_ledger.csv"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	out := map[uuid.UUID]bool{}
	for _, rec := range records[1:] {
		if rec[5] == "minted" {
			out[uuid.MustParse(rec[1])] = true
		}
	}
	return out
}

// versionOne is data version 1: the files without the rows version 2
// added (the ledger's minted ids) and without source codes.
func versionOne(t *testing.T) *referencedata.Dataset {
	t.Helper()
	minted := mintedIDs(t)
	v1 := load(t)
	v1.Version = 1
	v1.DepartmentSourceCodes = nil
	keep := func(rows []referencedata.Row) []referencedata.Row {
		return slices.DeleteFunc(rows, func(r referencedata.Row) bool { return minted[r.ID] })
	}
	v1.Universities, v1.Faculties, v1.Departments = keep(v1.Universities), keep(v1.Faculties), keep(v1.Departments)
	if len(v1.Universities) != 223 || len(v1.Faculties) != 2043 || len(v1.Departments) != 7567 {
		t.Fatalf("version 1 is %d universities, %d faculties, %d departments; want 223, 2043, 7567",
			len(v1.Universities), len(v1.Faculties), len(v1.Departments))
	}
	for _, r := range v1.Departments {
		if r.Degree != referencedata.DegreeBachelor {
			t.Fatalf("version 1 has the %s department %s", r.Degree, r.ID)
		}
	}
	return v1
}

// wantOnlyInserts checks a sync from version 1 to version 2: it inserts
// exactly version 2's rows and updates and retires nothing.
func wantOnlyInserts(t *testing.T, res referencedata.Result) {
	t.Helper()
	inserted := map[string]int{"universities": 8, "faculties": 979, "departments": 7144, "department_source_codes": 9254}
	for _, table := range referencedata.Tables() {
		c := of(res, table)
		if len(c.Inserted) != inserted[table] || len(c.Updated) != 0 || len(c.Retired) != 0 {
			t.Errorf("%s: %d inserted, %d updated, %d retired; want %d, 0, 0",
				table, len(c.Inserted), len(c.Updated), len(c.Retired), inserted[table])
		}
	}
	for _, c := range of(res, "departments").Inserted {
		if c.Degree != referencedata.DegreeAssociate {
			t.Errorf("inserted department %s %q is %s, want ASSOCIATE", c.ID, c.Name, c.Degree)
		}
	}
}

// TestSyncFromVersionOne drives the update this version is: a database
// that holds version 1 (the bachelor's programs of the Java snapshot)
// syncs to version 2 and only gains rows (ADR-0038): the universities,
// vocational schools, associate degree programs and source codes YÖK
// lists; nothing is renamed, brought back or retired, so no table comes
// near its retirement limit; the version is recorded; and a second run
// writes nothing. The database at version 1 is the template's with
// version 2's rows (the ledger's minted ids) taken out;
// TestMigrationGivesVersionOneItsDegree builds one through the migrations.
func TestSyncFromVersionOne(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	minted := slices.Collect(maps.Keys(mintedIDs(t)))
	for _, stmt := range []string{
		"DELETE FROM department_source_codes",
		"DELETE FROM departments WHERE degree = 'ASSOCIATE'",
		"DELETE FROM faculties WHERE id = ANY ($1::uuid[])",
		"DELETE FROM universities WHERE id = ANY ($1::uuid[])",
		"UPDATE reference_data_version SET version = 1",
	} {
		args := []any{}
		if strings.Contains(stmt, "$1") {
			args = append(args, minted)
		}
		if _, err := owner.Pool().Exec(t.Context(), stmt, args...); err != nil {
			t.Fatal(err)
		}
	}
	v1 := versionOne(t)
	// The database is at version 1: its files find nothing to do.
	if res, wrote, err := runSync(t, owner, v1, referencedata.Options{}); err != nil || wrote || res.Changed() {
		t.Fatalf("version 1 over version 1: %+v (wrote %v), %v", res, wrote, err)
	}

	res, _, err := runSync(t, owner, load(t), referencedata.Options{})
	if err != nil || res.DatabaseVersion != 1 || res.DataVersion != dataVersion {
		t.Fatalf("version 2 over version 1: %+v, %v", res, err)
	}
	wantOnlyInserts(t, res)
	if v := version(t, owner); v != dataVersion {
		t.Errorf("recorded version %d, want %d", v, dataVersion)
	}
	if res, wrote, err := runSync(t, owner, load(t), referencedata.Options{}); err != nil || wrote || res.Changed() {
		t.Fatalf("version 2 again: %+v (wrote %v), %v; want nothing written", res, wrote, err)
	}
}

// TestMigrationGivesVersionOneItsDegree runs 0048 over version 1 as 0047
// held it (ADR-0038): the migrations up to 0047, version 1's rows written
// as that schema takes them, then 0048. Every department is BACHELOR and
// version 2's sync only inserts. 0048's down deletes the associate degree
// programs, keeps the rest and the version, and the next up and sync put
// them back under their ids; an education record that names one stops the
// down with a message instead.
func TestMigrationGivesVersionOneItsDegree(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	database := dbtest.NewEmpty(t)
	migrator, err := db.NewMigrator(database.URL(db.RoleOwner), migrations.FS, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrator.Close() })
	step := func(up bool) error {
		t.Helper()
		if up {
			if _, err := migrator.Up(ctx); err != nil {
				return fmt.Errorf("up: %w", err)
			}
			return nil
		}
		if _, err := migrator.Down(ctx); err != nil {
			return fmt.Errorf("down: %w", err)
		}
		return nil
	}
	// Up to 0047: every migration, then 0048 down on its empty tables.
	if err := step(true); err != nil {
		t.Fatal(err)
	}
	if err := step(false); err != nil {
		t.Fatal(err)
	}
	owner := database.Open(t, db.RoleOwner)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := owner.Pool().Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	v1 := versionOne(t)
	type columns struct {
		ids, parents []uuid.UUID
		codes, names []string
	}
	cols := func(rows []referencedata.Row) columns {
		var c columns
		for _, r := range rows {
			c.ids, c.parents = append(c.ids, r.ID), append(c.parents, r.Parent)
			c.codes, c.names = append(c.codes, r.Code), append(c.names, r.Name)
		}
		return c
	}
	c := cols(v1.Countries)
	exec("INSERT INTO countries (id, code, name) SELECT * FROM unnest($1::uuid[], $2::text[], $3::text[])", c.ids, c.codes, c.names)
	for _, tc := range []struct {
		table, column string
		rows          []referencedata.Row
	}{
		{"cities", "country_id", v1.Cities}, {"districts", "city_id", v1.Districts},
		{"faculties", "university_id", v1.Faculties}, {"departments", "faculty_id", v1.Departments},
	} {
		if tc.table == "faculties" {
			c := cols(v1.Universities)
			exec("INSERT INTO universities (id, name) SELECT * FROM unnest($1::uuid[], $2::text[])", c.ids, c.names)
		}
		c := cols(tc.rows)
		exec("INSERT INTO "+tc.table+" (id, "+tc.column+", name) SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::text[])",
			c.ids, c.parents, c.names)
	}
	exec("INSERT INTO reference_data_version (singleton, version, revision, synced_at) VALUES (true, 1, 1, now())")

	// 0048 over version 1.
	if err := step(true); err != nil {
		t.Fatal(err)
	}
	var all, bachelor int
	if err := owner.Pool().QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE degree = 'BACHELOR') FROM departments").
		Scan(&all, &bachelor); err != nil || all != 7567 || bachelor != 7567 {
		t.Fatalf("after 0048: %d departments, %d BACHELOR (%v); want 7567 and 7567", all, bachelor, err)
	}
	if res, wrote, err := runSync(t, owner, v1, referencedata.Options{}); err != nil || wrote || res.Changed() {
		t.Fatalf("version 1 over 0048: %+v (wrote %v), %v; want nothing to do", res, wrote, err)
	}
	res, _, err := runSync(t, owner, load(t), referencedata.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wantOnlyInserts(t, res)

	// Down deletes the associate degree programs and keeps the version;
	// up and the sync put them back.
	if err := step(false); err != nil {
		t.Fatalf("0048 down: %v", err)
	}
	if err := owner.Pool().QueryRow(ctx, "SELECT count(*) FROM departments").Scan(&all); err != nil || all != 7567 || version(t, owner) != dataVersion {
		t.Fatalf("after 0048 down: %d departments (%v), version %d; want 7567 and %d", all, err, version(t, owner), dataVersion)
	}
	if err := step(true); err != nil {
		t.Fatal(err)
	}
	res, _, err = runSync(t, owner, load(t), referencedata.Options{})
	if err != nil || len(of(res, "departments").Inserted) != 7144 || len(of(res, "department_source_codes").Inserted) != 9254 ||
		len(of(res, "departments").Updated)+len(of(res, "departments").Retired) != 0 {
		t.Fatalf("the sync after down and up: %v, departments %+v", err, of(res, "departments"))
	}

	// An education record that names an associate degree program stops the
	// down, and nothing changes.
	user := uuid.New()
	exec(`INSERT INTO users (id, email, status, first_name, last_name, employer, email_verified, created_at)
		VALUES ($1, 'myo@example.com', 'ACTIVE', 'Ayşe', 'Yılmaz', false, true, now())`, user)
	exec(`INSERT INTO educations (id, user_id, university_id, faculty_id, department_id, degree, start_year, created_at)
		VALUES ($1, $2, '20b200c7-d3d3-4e77-ba67-2b01c96b92a8', '01a207cf-0348-7890-bf14-c44956448609',
		        '01a207cf-03cc-7a3c-9df7-92d5f5825c58', 'ASSOCIATE', 2018, now())`, uuid.New(), user)
	if err := step(false); err == nil || !strings.Contains(err.Error(), "education records name associate degree programs") {
		t.Fatalf("0048 down under a record: %v, want the refusal", err)
	}
	if err := owner.Pool().QueryRow(ctx, "SELECT count(*) FROM departments WHERE degree = 'ASSOCIATE'").Scan(&all); err != nil || all != 7144 {
		t.Fatalf("after the refused down: %d associate programs (%v), want 7144", all, err)
	}
}

// TestRetireLimitCountsRowsInUse: the limit is 5 % of the rows in use,
// not of every row. Retired rows from earlier years stay in the table; if
// they counted, a table with many of them would let a cut file through.
// Here 5,000 retired departments sit next to the 14,711 in use: retiring
// 736 is still above 735.
func TestRetireLimitCountsRowsInUse(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	if _, err := owner.Pool().Exec(t.Context(), `
		INSERT INTO departments (id, faculty_id, degree, name, retired_at)
		SELECT gen_random_uuid(), 'a8004de5-6f16-41ea-9841-42ed646684cf', 'BACHELOR', 'Eski ' || n, now()
		FROM generate_series(1, 5000) AS n`); err != nil {
		t.Fatal(err)
	}
	over := load(t)
	over.Departments = over.Departments[736:]
	_, _, err := runSync(t, owner, over, referencedata.Options{})
	var limit *referencedata.RetireLimitError
	if !errors.As(err, &limit) || limit.Active != 14711 || limit.Limit != 735 {
		t.Fatalf("736 retirements next to 5,000 retired rows: %v, want the limit of 735 over 14,711 rows in use", err)
	}
}

// TestAllowanceIsForOneVersion: an allowance names the data version it is
// for. One for another version, left over from an earlier release, lifts
// nothing: the limit stops the sync and names the ignored allowance; a
// sync it does not concern reports it as ignored.
func TestAllowanceIsForOneVersion(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	d := load(t)
	d.Departments = d.Departments[800:] // bachelor's programs, without codes
	stale := referencedata.Options{AllowRetire: map[string]referencedata.Allowance{"departments": {Rows: 800, Version: 3}}}
	_, _, err := runSync(t, owner, d, stale)
	var limit *referencedata.RetireLimitError
	if !errors.As(err, &limit) || limit.Allowed != 0 || limit.Ignored == nil || limit.Ignored.Version != 3 ||
		!strings.Contains(err.Error(), "another data version") || !strings.Contains(err.Error(), "departments:800@2") {
		t.Fatalf("an allowance for version 3 syncing version 2: %v", err)
	}
	if _, _, err := runSync(t, owner, d, allow(map[string]int{"departments": 800})); err != nil {
		t.Fatalf("the allowance for version 2: %v", err)
	}
	res, _, err := runSync(t, owner, load(t), stale)
	if err != nil || res.Ignored["departments"] != (referencedata.Allowance{Rows: 800, Version: 3}) {
		t.Fatalf("a sync the stale allowance does not concern: %+v, %v; want it reported as ignored", res.Ignored, err)
	}
}

// TestRepairMovesTheRevision: a sync that writes rows moves the revision,
// also under the same data version, so the lists' ETag changes and a
// client holding a 304 sees the repair; a sync that writes nothing does
// not move it.
func TestRepairMovesTheRevision(t *testing.T) {
	t.Parallel()

	owner := dbtest.New(t).Open(t, db.RoleOwner)
	if r := revision(t, owner); r != 1 {
		t.Fatalf("revision after the first load %d, want 1", r)
	}
	if _, _, err := runSync(t, owner, load(t), referencedata.Options{}); err != nil || revision(t, owner) != 1 {
		t.Fatalf("a sync with nothing to do: revision %d, %v; want 1", revision(t, owner), err)
	}
	if _, err := owner.Pool().Exec(t.Context(), "UPDATE universities SET name = 'EL İLE' WHERE id = 'a1c346d6-2872-4194-9b98-3f0ea5b838ec'"); err != nil {
		t.Fatal(err)
	}
	res, _, err := runSync(t, owner, load(t), referencedata.Options{})
	if err != nil || len(of(res, "universities").Updated) != 1 || revision(t, owner) != 2 || version(t, owner) != dataVersion {
		t.Fatalf("the repair: %+v, %v; revision %d, version %d; want revision 2 under version %d",
			res, err, revision(t, owner), version(t, owner), dataVersion)
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
	newer.Version = dataVersion + 1
	newer.Universities[0].Name = "SÜRÜM 3 ADI"
	res, _, err := runSync(t, owner, newer, referencedata.Options{})
	if err != nil || res.Skipped || res.DatabaseVersion != dataVersion || len(of(res, "universities").Updated) != 1 {
		t.Fatalf("version 3 over 2: %+v, %v", res, err)
	}
	if v := version(t, owner); v != dataVersion+1 {
		t.Fatalf("recorded version %d, want %d", v, dataVersion+1)
	}

	older := load(t) // version 2, the old name
	res, wrote, err := runSync(t, owner, older, referencedata.Options{})
	if err != nil || !res.Skipped || res.DataVersion != dataVersion || res.DatabaseVersion != dataVersion+1 || wrote || res.Changed() {
		t.Fatalf("version 2 over 3: %+v (wrote %v), %v; want it skipped", res, wrote, err)
	}
	var name string
	if err := owner.Pool().QueryRow(t.Context(), "SELECT name FROM universities WHERE id = $1", newer.Universities[0].ID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "SÜRÜM 3 ADI" || version(t, owner) != dataVersion+1 {
		t.Fatalf("after the older sync: %q, version %d; want version 3's data kept", name, version(t, owner))
	}

	// The same version repairs a drift and leaves the version as it is.
	if _, err := owner.Pool().Exec(t.Context(), "UPDATE universities SET name = 'EL İLE' WHERE id = $1", newer.Universities[0].ID); err != nil {
		t.Fatal(err)
	}
	res, _, err = runSync(t, owner, newer, referencedata.Options{})
	if err != nil || res.Skipped || len(of(res, "universities").Updated) != 1 || version(t, owner) != dataVersion+1 {
		t.Fatalf("version 3 over 3: %+v, %v", res, err)
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
	if active, _ := counts(t, owner); active["universities"] != 231 {
		t.Errorf("universities: %d in use after a refused sync; want 231", active["universities"])
	}
	_, _, err = runSync(t, owner, load(t), referencedata.Options{AllowRetire: map[string]referencedata.Allowance{"users": {Rows: 1, Version: dataVersion}}})
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
	d.Version = dataVersion + 1
	d.Departments = append(d.Departments, referencedata.Row{
		ID: uuid.New(), Parent: d.Faculties[0].ID, Degree: referencedata.DegreeBachelor, Name: "Eşzamanlı Bölüm",
	})
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
	if o.err != nil || o.res.Changed() || o.res.DatabaseVersion != dataVersion+1 {
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

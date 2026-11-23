package referencedata

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Tx is the part of a transaction Sync uses; db.Querier and pgx.Tx have
// it.
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options are a sync's exceptions to its limits.
type Options struct {
	// AllowRetire lifts a table's retirement limit up to Rows rows, for
	// the sync to data Version only: a release that means to retire many
	// rows says so by table and by the version that retires them, so an
	// allowance left in place does nothing in a later release.
	AllowRetire map[string]Allowance
}

// Allowance is one table's exception to the retirement limit.
type Allowance struct {
	Rows    int
	Version int
}

// String is the allowance as it is written: <rows>@<version>.
func (a Allowance) String() string {
	return strconv.Itoa(a.Rows) + "@" + strconv.Itoa(a.Version)
}

// ParseAllowRetire reads the --allow-retire value (and
// KOALA_REFERENCE_ALLOW_RETIRE): comma-separated "<table>:<rows>@<version>"
// entries, each table once and a reference table.
func ParseAllowRetire(s string) (map[string]Allowance, error) {
	out := map[string]Allowance{}
	if s == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(entry), ":")
		rows, version, okAt := strings.Cut(value, "@")
		n, errRows := strconv.Atoi(rows)
		v, errVersion := strconv.Atoi(version)
		switch {
		case !ok || !okAt || errRows != nil || errVersion != nil || n < 0 || v < 1:
			return nil, fmt.Errorf("referencedata: allow-retire %q: want <table>:<rows>@<data version>", entry)
		case !slices.Contains(Tables(), name):
			return nil, fmt.Errorf("referencedata: allow-retire %q: %s is not a reference table %v", entry, name, Tables())
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("referencedata: allow-retire: %s given twice", name)
		}
		out[name] = Allowance{Rows: n, Version: v}
	}
	return out, nil
}

// RetireLimit is how many rows a sync may retire from a table that has
// active rows in use without being allowed to: 5 % of them, at least 10. A
// file cut short or emptied by mistake (a header and nothing else) would
// retire them all; the limit stops it before anything is written. The
// first load retires nothing.
func RetireLimit(active int) int {
	return max(10, active*5/100)
}

// RetireLimitError is a sync stopped by a table's retirement limit.
type RetireLimitError struct {
	Table   string
	Retire  int // rows the sync would retire
	Active  int // rows in use before it
	Limit   int // RetireLimit(Active)
	Allowed int // the allowance for this data version, 0 if none
	Version int // the data version being synced
	// Ignored is an allowance given for another data version.
	Ignored *Allowance
}

// Error names the table, the counts and the allowance that lets it
// through.
func (e *RetireLimitError) Error() string {
	msg := fmt.Sprintf("referencedata: %s: the files would retire %d of its %d rows in use, above the limit of %d "+
		"(allowed %d); nothing was written. If that is meant, allow it: %s:%d@%d",
		e.Table, e.Retire, e.Active, e.Limit, e.Allowed, e.Table, e.Retire, e.Version)
	if e.Ignored != nil {
		msg += fmt.Sprintf(" (the allowance %s:%s names another data version and was ignored)", e.Table, e.Ignored)
	}
	return msg
}

// Change is one row a sync writes.
type Change struct {
	// ID is the row's id; uuid.Nil in department_source_codes, whose row
	// is its Code.
	ID uuid.UUID
	// Parent is the row's parent (uuid.Nil in a table without one); a
	// source code's department.
	Parent uuid.UUID
	// Name and Code are the row's after the sync (Code: a country's code,
	// or a source code). Degree is a department's.
	Name   string
	Code   string
	Degree string
	// OldName and OldCode are an update's values before it, set only when
	// they change.
	OldName string
	OldCode string
	// Restored: an update brings a retired row back into use.
	Restored bool
}

// TableResult is what Sync does to one table.
type TableResult struct {
	Table     string
	Inserted  []Change
	Updated   []Change
	Retired   []Change
	Unchanged int
	// Active is the number of rows in use before the sync.
	Active int
}

// Result is what Sync did.
type Result struct {
	// DataVersion is the files' version, DatabaseVersion the one the
	// tables held before the sync (0 before the first).
	DataVersion     int
	DatabaseVersion int
	// Skipped: the database holds a newer version than the files, which is
	// what a rolled-back release meets. Nothing was written; the newer data
	// stays.
	Skipped bool
	// Ignored are the allowances given for another data version than the
	// files': left over from an earlier release, they lift nothing.
	Ignored map[string]Allowance
	Tables  []TableResult
}

// Changed reports whether Sync wrote any row.
func (r Result) Changed() bool {
	for _, t := range r.Tables {
		if len(t.Inserted)+len(t.Updated)+len(t.Retired) > 0 {
			return true
		}
	}
	return false
}

// lockClass is the advisory lock that serializes two syncs of one
// database (two releases, a release and a dry run): the second waits for
// the first to end and plans on what it committed. "RD", apart from the
// other two-key locks.
const lockClass = 0x5244

// Sync brings the reference tables in line with d in tx and returns what
// it did, row by row. The caller commits, or rolls back for a dry run.
//
//  1. d is validated (Validate).
//  2. The advisory lock is taken.
//  3. Version: if the database holds a newer version than d, nothing is
//     written and the result says Skipped; the same version syncs (a
//     second run finds nothing to do, a drift is repaired); a newer one
//     syncs and is recorded.
//  4. Plan: every table is read and compared with d, parents first. A row
//     the database has under another parent than d gives is refused: a
//     faculty that moves to another university is another faculty, with a
//     new id, since education records hold the pair (ADR-0036 Karar 6).
//     So is a department whose degree changes (an associate degree
//     program is not a bachelor's program; ADR-0038). The source codes are
//     planned last, by (code, department), under the same rules: a dropped
//     pair is retired, a listed one back in use; a code given to another
//     department retires its old pair and inserts the new one.
//  5. Limit: a table that would retire more than RetireLimit of its rows
//     in use, and more than opts allows for d's version, stops the sync
//     (RetireLimitError).
//  6. Write: updates, inserts and retirements, exactly the planned rows;
//     then the version and its revision, if anything was written or the
//     version is new.
//
// Steps 1 to 5 write nothing. An allowance for another data version than
// d's is ignored and named in the result (Result.Ignored) and in a
// RetireLimitError.
func Sync(ctx context.Context, tx Tx, d *Dataset, opts Options) (Result, error) {
	if err := d.Validate(); err != nil {
		return Result{}, err
	}
	allowed := map[string]int{}
	ignored := map[string]Allowance{}
	for name, a := range opts.AllowRetire {
		if !slices.Contains(Tables(), name) || a.Rows < 0 || a.Version < 1 {
			return Result{}, fmt.Errorf("referencedata: allow-retire %s:%s: not a reference table, a row count and a version", name, a)
		}
		if a.Version == d.Version {
			allowed[name] = a.Rows
		} else {
			ignored[name] = a
		}
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1::integer, 0)", lockClass); err != nil {
		return Result{}, fmt.Errorf("referencedata: lock: %w", err)
	}
	var current int
	if err := tx.QueryRow(ctx, "SELECT coalesce((SELECT version FROM reference_data_version), 0)").Scan(&current); err != nil {
		return Result{}, fmt.Errorf("referencedata: read the version: %w", err)
	}
	res := Result{DataVersion: d.Version, DatabaseVersion: current, Ignored: ignored}
	if d.Version < current {
		res.Skipped = true
		return res, nil
	}

	for _, t := range tables() {
		p, err := plan(ctx, tx, t, *t.rows(d))
		if err != nil {
			return Result{}, err
		}
		res.Tables = append(res.Tables, p)
	}
	codes, err := planSourceCodes(ctx, tx, d.DepartmentSourceCodes)
	if err != nil {
		return Result{}, err
	}
	res.Tables = append(res.Tables, codes)
	for _, p := range res.Tables {
		if limit := RetireLimit(p.Active); len(p.Retired) > limit && len(p.Retired) > allowed[p.Table] {
			e := &RetireLimitError{
				Table: p.Table, Retire: len(p.Retired), Active: p.Active, Limit: limit, Allowed: allowed[p.Table], Version: d.Version,
			}
			if a, ok := ignored[p.Table]; ok {
				e.Ignored = &a
			}
			return Result{}, e
		}
	}
	for i, t := range tables() {
		if err := apply(ctx, tx, t, res.Tables[i]); err != nil {
			return Result{}, err
		}
	}
	if err := applySourceCodes(ctx, tx, codes); err != nil {
		return Result{}, err
	}
	// A new version, or a repair under the same one, moves the revision:
	// the lists' ETag changes and a client that holds the old lists gets
	// the new ones. A sync that writes nothing writes no revision either.
	if d.Version > current || res.Changed() {
		_, err := tx.Exec(ctx, `
			INSERT INTO reference_data_version (singleton, version, revision, synced_at) VALUES (true, $1, 1, now())
			ON CONFLICT (singleton) DO UPDATE
			SET version = EXCLUDED.version, revision = reference_data_version.revision + 1, synced_at = EXCLUDED.synced_at`, d.Version)
		if err != nil {
			return Result{}, fmt.Errorf("referencedata: record the version: %w", err)
		}
	}
	return res, nil
}

// stored is a table's row as the database has it.
type stored struct {
	parent  uuid.UUID
	code    string
	degree  string
	name    string
	retired bool
}

// plan reads one table and compares it with rows.
func plan(ctx context.Context, tx Tx, t table, rows []Row) (TableResult, error) {
	name := pgx.Identifier{t.name}.Sanitize()
	parent, code, degree := "'00000000-0000-0000-0000-000000000000'::uuid", "''", "''"
	if t.parent != "" {
		parent = pgx.Identifier{t.column}.Sanitize()
	}
	if t.code {
		code = "code"
	}
	if t.degree {
		degree = "degree"
	}
	query, err := tx.Query(ctx, "SELECT id, "+parent+", "+code+", "+degree+", name, retired_at IS NOT NULL FROM "+name)
	if err != nil {
		return TableResult{}, fmt.Errorf("referencedata: read %s: %w", t.name, err)
	}
	have := map[uuid.UUID]stored{}
	var id uuid.UUID
	var s stored
	_, err = pgx.ForEachRow(query, []any{&id, &s.parent, &s.code, &s.degree, &s.name, &s.retired}, func() error {
		have[id] = s
		return nil
	})
	if err != nil {
		return TableResult{}, fmt.Errorf("referencedata: read %s: %w", t.name, err)
	}

	out := TableResult{Table: t.name}
	listed := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		listed[r.ID] = true
		h, ok := have[r.ID]
		switch {
		case !ok:
			out.Inserted = append(out.Inserted, Change{ID: r.ID, Parent: r.Parent, Name: r.Name, Code: r.Code, Degree: r.Degree})
		case h.parent != r.Parent:
			return TableResult{}, fmt.Errorf("referencedata: %s %s: the files give it another %s; a moved row is a new row with a new id",
				t.name, r.ID, t.column)
		case h.degree != r.Degree:
			return TableResult{}, fmt.Errorf("referencedata: %s %s: the files give it the degree %s, the database %s; "+
				"a program of another degree is a new row with a new id", t.name, r.ID, r.Degree, h.degree)
		case h.name != r.Name || h.code != r.Code || h.retired:
			c := Change{ID: r.ID, Parent: r.Parent, Name: r.Name, Code: r.Code, Degree: r.Degree, Restored: h.retired}
			if h.name != r.Name {
				c.OldName = h.name
			}
			if h.code != r.Code {
				c.OldCode = h.code
			}
			out.Updated = append(out.Updated, c)
		default:
			out.Unchanged++
		}
	}
	for hid, h := range have {
		if h.retired {
			continue
		}
		out.Active++
		if !listed[hid] {
			out.Retired = append(out.Retired, Change{ID: hid, Parent: h.parent, Name: h.name, Code: h.code, Degree: h.degree})
		}
	}
	slices.SortFunc(out.Retired, func(a, b Change) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}

// apply writes one table's plan. Every statement must write exactly the
// planned rows: under the advisory lock nothing else writes the reference
// tables, so a difference is a bug or a hand edit, and the transaction
// rolls back.
func apply(ctx context.Context, tx Tx, t table, p TableResult) error {
	name := pgx.Identifier{t.name}.Sanitize()
	columns := func(cs []Change) (ids, parents []uuid.UUID, codes, degrees, names []string) {
		for _, c := range cs {
			ids, parents = append(ids, c.ID), append(parents, c.Parent)
			codes, degrees, names = append(codes, c.Code), append(degrees, c.Degree), append(names, c.Name)
		}
		return ids, parents, codes, degrees, names
	}
	exact := func(what string, tag pgconn.CommandTag, want int) error {
		if got := tag.RowsAffected(); got != int64(want) {
			return fmt.Errorf("referencedata: %s: %s %d rows, planned %d; the table changed under the sync", t.name, what, got, want)
		}
		return nil
	}

	if len(p.Updated) > 0 {
		ids, _, codes, _, names := columns(p.Updated)
		set := "name = i.name, retired_at = NULL"
		if t.code {
			set = "code = i.code, " + set
		}
		tag, err := tx.Exec(ctx, `UPDATE `+name+` AS t SET `+set+`
			FROM unnest($1::uuid[], $2::text[], $3::text[]) AS i (id, code, name) WHERE t.id = i.id`, ids, codes, names)
		if err != nil {
			return fmt.Errorf("referencedata: %s: update: %w", t.name, err)
		}
		if err := exact("updated", tag, len(p.Updated)); err != nil {
			return err
		}
	}
	if len(p.Inserted) > 0 {
		ids, parents, codes, degrees, names := columns(p.Inserted)
		var tag pgconn.CommandTag
		var err error
		switch {
		case t.degree:
			column := pgx.Identifier{t.column}.Sanitize()
			tag, err = tx.Exec(ctx, `INSERT INTO `+name+` (id, `+column+`, degree, name)
				SELECT i.id, i.parent, i.degree, i.name
				FROM unnest($1::uuid[], $2::uuid[], $3::text[], $4::text[]) AS i (id, parent, degree, name)`,
				ids, parents, degrees, names)
		case t.code:
			tag, err = tx.Exec(ctx, `INSERT INTO `+name+` (id, code, name)
				SELECT i.id, i.code, i.name FROM unnest($1::uuid[], $2::text[], $3::text[]) AS i (id, code, name)`, ids, codes, names)
		case t.parent != "":
			column := pgx.Identifier{t.column}.Sanitize()
			tag, err = tx.Exec(ctx, `INSERT INTO `+name+` (id, `+column+`, name)
				SELECT i.id, i.parent, i.name FROM unnest($1::uuid[], $2::uuid[], $3::text[]) AS i (id, parent, name)`, ids, parents, names)
		default:
			tag, err = tx.Exec(ctx, `INSERT INTO `+name+` (id, name)
				SELECT i.id, i.name FROM unnest($1::uuid[], $2::text[]) AS i (id, name)`, ids, names)
		}
		if err != nil {
			return fmt.Errorf("referencedata: %s: insert: %w", t.name, err)
		}
		if err := exact("inserted", tag, len(p.Inserted)); err != nil {
			return err
		}
	}
	if len(p.Retired) > 0 {
		ids, _, _, _, _ := columns(p.Retired)
		tag, err := tx.Exec(ctx, `UPDATE `+name+` SET retired_at = now() WHERE id = ANY ($1::uuid[]) AND retired_at IS NULL`, ids)
		if err != nil {
			return fmt.Errorf("referencedata: %s: retire: %w", t.name, err)
		}
		if err := exact("retired", tag, len(p.Retired)); err != nil {
			return err
		}
	}
	return nil
}

// codePair is a row of department_source_codes: a code and the
// department it names. The pair never changes; a code ÖSYM gives to another
// program (decided in tools/id_decisions.csv, ADR-0038) is another pair.
type codePair struct {
	code       string
	department uuid.UUID
}

// planSourceCodes reads department_source_codes and compares it with
// codes, as plan does a table, by (code, department): a new pair is
// inserted, a retired one that is listed again is brought back, one no
// longer listed is retired. A code given to another department is the old
// pair retired and the new one inserted, both in the result; the table
// holds at most one pair in use per code (a partial unique index), and
// applySourceCodes retires before it inserts. Its Changes carry the code in
// Code and the department in Parent; ID is uuid.Nil.
func planSourceCodes(ctx context.Context, tx Tx, codes []SourceCode) (TableResult, error) {
	query, err := tx.Query(ctx, "SELECT code, department_id, retired_at IS NOT NULL FROM "+sourceCodes)
	if err != nil {
		return TableResult{}, fmt.Errorf("referencedata: read %s: %w", sourceCodes, err)
	}
	have := map[codePair]bool{} // the pair's retired
	var p codePair
	var retired bool
	_, err = pgx.ForEachRow(query, []any{&p.code, &p.department, &retired}, func() error {
		have[p] = retired
		return nil
	})
	if err != nil {
		return TableResult{}, fmt.Errorf("referencedata: read %s: %w", sourceCodes, err)
	}
	out := TableResult{Table: sourceCodes}
	listed := make(map[codePair]bool, len(codes))
	for _, c := range codes {
		p := codePair{code: c.Code, department: c.Department}
		listed[p] = true
		wasRetired, ok := have[p]
		switch {
		case !ok:
			out.Inserted = append(out.Inserted, Change{Code: c.Code, Parent: c.Department})
		case wasRetired:
			out.Updated = append(out.Updated, Change{Code: c.Code, Parent: c.Department, Restored: true})
		default:
			out.Unchanged++
		}
	}
	for hp, wasRetired := range have {
		if wasRetired {
			continue
		}
		out.Active++
		if !listed[hp] {
			out.Retired = append(out.Retired, Change{Code: hp.code, Parent: hp.department})
		}
	}
	slices.SortFunc(out.Retired, func(a, b Change) int {
		return strings.Compare(a.Code+a.Parent.String(), b.Code+b.Parent.String())
	})
	return out, nil
}

// applySourceCodes writes the plan of department_source_codes, after the
// departments' (a new pair names a new department), with apply's rule:
// every statement writes exactly the planned rows. Retirements come first:
// a code given to another department leaves its old pair before the new
// one is in use, which the partial unique index requires.
func applySourceCodes(ctx context.Context, tx Tx, p TableResult) error {
	columns := func(cs []Change) (codes []string, departments []uuid.UUID) {
		for _, c := range cs {
			codes, departments = append(codes, c.Code), append(departments, c.Parent)
		}
		return codes, departments
	}
	exact := func(what string, tag pgconn.CommandTag, want int) error {
		if got := tag.RowsAffected(); got != int64(want) {
			return fmt.Errorf("referencedata: %s: %s %d rows, planned %d; the table changed under the sync", sourceCodes, what, got, want)
		}
		return nil
	}
	if len(p.Retired) > 0 {
		codes, departments := columns(p.Retired)
		tag, err := tx.Exec(ctx, `UPDATE `+sourceCodes+` AS t SET retired_at = now()
			FROM unnest($1::text[], $2::uuid[]) AS i (code, department)
			WHERE t.code = i.code AND t.department_id = i.department AND t.retired_at IS NULL`, codes, departments)
		if err != nil {
			return fmt.Errorf("referencedata: %s: retire: %w", sourceCodes, err)
		}
		if err := exact("retired", tag, len(p.Retired)); err != nil {
			return err
		}
	}
	if len(p.Updated) > 0 {
		codes, departments := columns(p.Updated)
		tag, err := tx.Exec(ctx, `UPDATE `+sourceCodes+` AS t SET retired_at = NULL
			FROM unnest($1::text[], $2::uuid[]) AS i (code, department)
			WHERE t.code = i.code AND t.department_id = i.department AND t.retired_at IS NOT NULL`, codes, departments)
		if err != nil {
			return fmt.Errorf("referencedata: %s: restore: %w", sourceCodes, err)
		}
		if err := exact("restored", tag, len(p.Updated)); err != nil {
			return err
		}
	}
	if len(p.Inserted) > 0 {
		codes, departments := columns(p.Inserted)
		tag, err := tx.Exec(ctx, `INSERT INTO `+sourceCodes+` (code, department_id)
			SELECT i.code, i.department FROM unnest($1::text[], $2::uuid[]) AS i (code, department)`, codes, departments)
		if err != nil {
			return fmt.Errorf("referencedata: %s: insert: %w", sourceCodes, err)
		}
		if err := exact("inserted", tag, len(p.Inserted)); err != nil {
			return err
		}
	}
	return nil
}

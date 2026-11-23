// Package referencedata holds the reference data of the M4 tables and the
// one function that brings a database in line with it (ADR-0037).
//
// The data is six CSV files next to this file: countries, cities and
// districts; universities, faculties and departments. They are versioned
// with the code and embedded in the binary (README.md has their source,
// date and license). A file lists the rows that are in use; a row's id
// never changes.
//
// Sync makes the tables say what the files say, in the caller's
// transaction: a row whose id is new is inserted, a row whose name (or a
// country's code) changed is updated, a retired row that is back is
// brought back, and a row the files no longer list is retired
// (retired_at), never deleted, because records name it. Running it again
// changes nothing. `koala migrate up` runs it after the migrations, as
// koala_owner, so a release that changes a file changes the tables;
// `koala reference sync --dry-run` shows what it would do. The test
// template database runs it too (internal/platform/dbtest).
package referencedata

import (
	"bytes"
	"context"
	"embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// FS holds the CSV files at its root.
//
//go:embed *.csv
var FS embed.FS

// Row is one reference row as its file gives it.
type Row struct {
	ID uuid.UUID
	// Parent is the row's parent (a city's country, a faculty's
	// university); uuid.Nil in a table without one.
	Parent uuid.UUID
	// Code is a country's ISO 3166-1 alpha-2 code; empty elsewhere.
	Code string
	Name string
}

// Dataset is the content of the six files.
type Dataset struct {
	Countries    []Row
	Cities       []Row
	Districts    []Row
	Universities []Row
	Faculties    []Row
	Departments  []Row
}

// table describes one reference table and its file.
type table struct {
	// name is the table and, with .csv, the file.
	name string
	// parent is the parent table and column ("" for a table without one).
	parent, column string
	// code: the table has a code column (countries).
	code bool
	rows func(*Dataset) *[]Row
}

// tables lists the reference tables, every parent before its children.
func tables() []table {
	return []table{
		{name: "countries", code: true, rows: func(d *Dataset) *[]Row { return &d.Countries }},
		{name: "cities", parent: "countries", column: "country_id", rows: func(d *Dataset) *[]Row { return &d.Cities }},
		{name: "districts", parent: "cities", column: "city_id", rows: func(d *Dataset) *[]Row { return &d.Districts }},
		{name: "universities", rows: func(d *Dataset) *[]Row { return &d.Universities }},
		{name: "faculties", parent: "universities", column: "university_id", rows: func(d *Dataset) *[]Row { return &d.Faculties }},
		{name: "departments", parent: "faculties", column: "faculty_id", rows: func(d *Dataset) *[]Row { return &d.Departments }},
	}
}

// Tables lists the reference tables in the order Sync writes them.
func Tables() []string {
	ts := tables()
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.name)
	}
	return out
}

// header is the first line a table's file must have.
func (t table) header() []string {
	switch {
	case t.code:
		return []string{"id", "code", "name"}
	case t.parent != "":
		return []string{"id", t.column, "name"}
	default:
		return []string{"id", "name"}
	}
}

// maxName is the names' length limit, the tables' CHECK.
const maxName = 200

var countryCode = regexp.MustCompile(`^[A-Z]{2}$`)

// Load reads and validates the embedded files.
func Load() (*Dataset, error) {
	var d Dataset
	for _, t := range tables() {
		data, err := FS.ReadFile(t.name + ".csv")
		if err != nil {
			return nil, fmt.Errorf("referencedata: %w", err)
		}
		rows, err := parse(t, data)
		if err != nil {
			return nil, err
		}
		*t.rows(&d) = rows
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// parse reads one file: an exact header, then one row per record.
func parse(t table, data []byte) ([]Row, error) {
	r := csv.NewReader(bytes.NewReader(data))
	want := t.header()
	r.FieldsPerRecord = len(want)
	head, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("referencedata: %s.csv: header: %w", t.name, err)
	}
	if !slices.Equal(head, want) {
		return nil, fmt.Errorf("referencedata: %s.csv: header %q, want %q", t.name, head, want)
	}
	var rows []Row
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("referencedata: %s.csv: %w", t.name, err)
		}
		id, err := uuid.Parse(rec[0])
		if err != nil {
			return nil, fmt.Errorf("referencedata: %s.csv: id %q: %w", t.name, rec[0], err)
		}
		row := Row{ID: id, Name: rec[len(rec)-1]}
		switch {
		case t.code:
			row.Code = rec[1]
		case t.parent != "":
			if row.Parent, err = uuid.Parse(rec[1]); err != nil {
				return nil, fmt.Errorf("referencedata: %s.csv: %s %q: %w", t.name, t.column, rec[1], err)
			}
		}
		rows = append(rows, row)
	}
}

// Validate checks what the database would refuse, before anything is
// written and with the row named: ids are set and unique within a table,
// every row's parent is a row of the parent table, names are 1 to 200
// characters with no control character and no surrounding space, and
// country codes are two capital letters and unique. A row whose parent
// the files drop has to be dropped too: retiring a university retires its
// faculties.
func (d *Dataset) Validate() error {
	ids := map[string]map[uuid.UUID]bool{}
	for _, t := range tables() {
		seen := map[uuid.UUID]bool{}
		codes := map[string]bool{}
		for _, r := range *t.rows(d) {
			where := fmt.Sprintf("referencedata: %s %s", t.name, r.ID)
			if r.ID == uuid.Nil {
				return fmt.Errorf("referencedata: %s: a row without an id (%q)", t.name, r.Name)
			}
			if seen[r.ID] {
				return fmt.Errorf("%s: listed twice", where)
			}
			seen[r.ID] = true
			if err := checkName(r.Name); err != nil {
				return fmt.Errorf("%s: name %q: %w", where, r.Name, err)
			}
			if t.parent != "" && !ids[t.parent][r.Parent] {
				return fmt.Errorf("%s: %s %s is not a row of %s", where, t.column, r.Parent, t.parent)
			}
			if t.parent == "" && r.Parent != uuid.Nil {
				return fmt.Errorf("%s: %s has no parent", where, t.name)
			}
			if t.code {
				if !countryCode.MatchString(r.Code) {
					return fmt.Errorf("%s: code %q is not ISO 3166-1 alpha-2", where, r.Code)
				}
				if codes[r.Code] {
					return fmt.Errorf("%s: code %s listed twice", where, r.Code)
				}
				codes[r.Code] = true
			} else if r.Code != "" {
				return fmt.Errorf("%s: %s has no code", where, t.name)
			}
		}
		ids[t.name] = seen
	}
	return nil
}

func checkName(name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case !utf8.ValidString(name):
		return errors.New("not UTF-8")
	case n == 0 || n > maxName:
		return fmt.Errorf("%d characters, want 1 to %d", n, maxName)
	case strings.TrimSpace(name) != name:
		return errors.New("surrounding space")
	case strings.ContainsFunc(name, unicode.IsControl):
		return errors.New("control character")
	}
	return nil
}

// Counts is what Sync did to one table. Unchanged rows are listed and
// already as listed.
type Counts struct {
	Inserted  int64
	Updated   int64
	Retired   int64
	Unchanged int64
}

// TableCounts is one table's Counts.
type TableCounts struct {
	Table string
	Counts
}

// Summary is what Sync did, one entry per table in Tables order.
type Summary []TableCounts

// Changed reports whether Sync wrote anything.
func (s Summary) Changed() bool {
	for _, t := range s {
		if t.Inserted+t.Updated+t.Retired > 0 {
			return true
		}
	}
	return false
}

// Tx is the part of a transaction Sync uses; db.Querier and pgx.Tx have
// it.
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockClass is the advisory lock that serializes two syncs of one
// database (two releases, a release and a dry run): the second waits for
// the first to end and then finds nothing to do. "RD", apart from the
// other two-key locks.
const lockClass = 0x5244

// Sync brings the reference tables in line with d in tx, parents first,
// and returns what it did. d is validated first. A row the database has
// under another parent than d gives is refused: a faculty that moves to
// another university is another faculty, with a new id, since education
// records hold the pair (ADR-0036 Karar 6). The caller commits, or rolls
// back for a dry run.
func Sync(ctx context.Context, tx Tx, d *Dataset) (Summary, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1::integer, 0)", lockClass); err != nil {
		return nil, fmt.Errorf("referencedata: lock: %w", err)
	}
	out := make(Summary, 0, len(tables()))
	for _, t := range tables() {
		c, err := syncTable(ctx, tx, t, *t.rows(d))
		if err != nil {
			return nil, err
		}
		out = append(out, TableCounts{Table: t.name, Counts: c})
	}
	return out, nil
}

// statements are one table's SQL. The table and column names come from
// tables(), never from input.
type statements struct {
	moved, update, insert, retire string
}

func (t table) statements() statements {
	name := pgx.Identifier{t.name}.Sanitize()
	var s statements
	switch {
	case t.code:
		s.update = `UPDATE ` + name + ` AS t SET code = i.code, name = i.name, retired_at = NULL
			FROM unnest($1::uuid[], $2::text[], $3::text[]) AS i (id, code, name)
			WHERE t.id = i.id AND (t.code, t.name, t.retired_at IS NULL) IS DISTINCT FROM (i.code, i.name, true)`
		s.insert = `INSERT INTO ` + name + ` (id, code, name)
			SELECT i.id, i.code, i.name FROM unnest($1::uuid[], $2::text[], $3::text[]) AS i (id, code, name)
			ON CONFLICT (id) DO NOTHING`
	case t.parent != "":
		column := pgx.Identifier{t.column}.Sanitize()
		s.moved = `SELECT t.id FROM ` + name + ` AS t
			JOIN unnest($1::uuid[], $2::uuid[]) AS i (id, parent) ON i.id = t.id
			WHERE t.` + column + ` <> i.parent LIMIT 1`
		s.update = `UPDATE ` + name + ` AS t SET name = i.name, retired_at = NULL
			FROM unnest($1::uuid[], $2::uuid[], $3::text[]) AS i (id, parent, name)
			WHERE t.id = i.id AND (t.name, t.retired_at IS NULL) IS DISTINCT FROM (i.name, true)`
		s.insert = `INSERT INTO ` + name + ` (id, ` + column + `, name)
			SELECT i.id, i.parent, i.name FROM unnest($1::uuid[], $2::uuid[], $3::text[]) AS i (id, parent, name)
			ON CONFLICT (id) DO NOTHING`
	default:
		s.update = `UPDATE ` + name + ` AS t SET name = i.name, retired_at = NULL
			FROM unnest($1::uuid[], $2::text[], $3::text[]) AS i (id, unused, name)
			WHERE t.id = i.id AND (t.name, t.retired_at IS NULL) IS DISTINCT FROM (i.name, true)`
		s.insert = `INSERT INTO ` + name + ` (id, name)
			SELECT i.id, i.name FROM unnest($1::uuid[], $2::text[], $3::text[]) AS i (id, unused, name)
			ON CONFLICT (id) DO NOTHING`
	}
	// NOT EXISTS over unnest is a hashed anti-join; `id <> ALL($1)` would
	// compare every row with every id.
	s.retire = `UPDATE ` + name + ` AS t SET retired_at = now()
		WHERE t.retired_at IS NULL AND NOT EXISTS (SELECT FROM unnest($1::uuid[]) AS i (id) WHERE i.id = t.id)`
	return s
}

// syncTable writes one table: refuse a moved row, update, insert, retire.
// The update runs before the insert, so a row is counted once.
func syncTable(ctx context.Context, tx Tx, t table, rows []Row) (Counts, error) {
	ids := make([]uuid.UUID, len(rows))
	parents := make([]uuid.UUID, len(rows))
	codes := make([]string, len(rows))
	names := make([]string, len(rows))
	for i, r := range rows {
		ids[i], parents[i], codes[i], names[i] = r.ID, r.Parent, r.Code, r.Name
	}
	// The second array is the code, the parent or (a table with neither)
	// an unused column of the same length, so every shape takes the same
	// three arrays.
	var second any = codes
	if t.parent != "" {
		second = parents
	}
	s := t.statements()

	if s.moved != "" {
		var moved uuid.UUID
		err := tx.QueryRow(ctx, s.moved, ids, parents).Scan(&moved)
		switch {
		case err == nil:
			return Counts{}, fmt.Errorf("referencedata: %s %s: the files give it another %s; a moved row is a new row with a new id",
				t.name, moved, t.column)
		case !errors.Is(err, pgx.ErrNoRows):
			return Counts{}, fmt.Errorf("referencedata: %s: look for moved rows: %w", t.name, err)
		}
	}
	var c Counts
	tag, err := tx.Exec(ctx, s.update, ids, second, names)
	if err != nil {
		return Counts{}, fmt.Errorf("referencedata: %s: update: %w", t.name, err)
	}
	c.Updated = tag.RowsAffected()
	tag, err = tx.Exec(ctx, s.insert, ids, second, names)
	if err != nil {
		return Counts{}, fmt.Errorf("referencedata: %s: insert: %w", t.name, err)
	}
	c.Inserted = tag.RowsAffected()
	tag, err = tx.Exec(ctx, s.retire, ids)
	if err != nil {
		return Counts{}, fmt.Errorf("referencedata: %s: retire: %w", t.name, err)
	}
	c.Retired = tag.RowsAffected()
	c.Unchanged = int64(len(rows)) - c.Updated - c.Inserted
	return c, nil
}

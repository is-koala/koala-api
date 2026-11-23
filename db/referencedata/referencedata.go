// Package referencedata holds the reference data of the M4 tables and the
// one function that brings a database in line with it (ADR-0037).
//
// The data is CSV files next to this file: countries, cities and
// districts; universities, faculties and departments, and the source codes
// of the departments (department_source_codes: ÖSYM program codes), with a
// version number (VERSION) that grows with every change. They are versioned with
// the code and embedded in the binary (README.md has their source, date,
// license and cleanup). A file lists the rows that are in use; a row's id
// never changes.
//
// Sync (sync.go) makes the tables say what the files say, in the caller's
// transaction: a row whose id is new is inserted, a row whose name (or a
// country's code) changed is updated, a retired row that is back is
// brought back, and a row the files no longer list is retired
// (retired_at), never deleted, because records name it. Running it again
// changes nothing. `koala migrate up` runs it after the migrations, as
// koala_owner, so a release that changes a file changes the tables;
// `koala reference sync --dry-run` shows what it would do, row by row. The
// test template database runs it too (internal/platform/dbtest).
package referencedata

import (
	"bytes"
	"embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// FS holds the CSV files and VERSION at its root.
//
//go:embed *.csv VERSION
var FS embed.FS

// SourceFS holds this package's Go files: the sync's logic. The test
// template's name hashes it with FS, so a change of either rebuilds the
// template (internal/platform/dbtest).
//
//go:embed *.go
var SourceFS embed.FS

// Row is one reference row as its file gives it.
type Row struct {
	ID uuid.UUID
	// Parent is the row's parent (a city's country, a faculty's
	// university); uuid.Nil in a table without one.
	Parent uuid.UUID
	// Code is a country's ISO 3166-1 alpha-2 code; empty elsewhere.
	Code string
	// Degree is a department's degree, DegreeAssociate or DegreeBachelor
	// (ADR-0038); empty elsewhere.
	Degree string
	Name   string
}

// The degrees of a department (a program): an associate degree program
// (önlisans) or a bachelor's program (lisans). A department's degree is
// part of what it is, like its faculty: Sync refuses a row that changes
// it (ADR-0038).
const (
	DegreeAssociate = "ASSOCIATE"
	DegreeBachelor  = "BACHELOR"
)

// SourceCode is one source code of a department: an ÖSYM program code
// (YÖK Atlas's "program kodu", nine digits). A department merged from a
// program's quota and fee variants has the code of each (ADR-0038). The
// files list a code once. In the table a row is a (code, department) pair
// that never changes: a code given to another program (decided in the id
// ledger, tools/id_decisions.csv) retires its old pair and adds a new one.
// The department's id comes from the ledger, never from the code.
type SourceCode struct {
	Code       string
	Department uuid.UUID
}

// Dataset is the content of the files.
type Dataset struct {
	// Version is VERSION: it grows with every change of the files, so a
	// database that holds a newer version is never taken back by an older
	// binary (Sync).
	Version      int
	Countries    []Row
	Cities       []Row
	Districts    []Row
	Universities []Row
	Faculties    []Row
	Departments  []Row
	// DepartmentSourceCodes is department_source_codes.csv.
	DepartmentSourceCodes []SourceCode
}

// table describes one reference table and its file.
type table struct {
	// name is the table and, with .csv, the file.
	name string
	// parent is the parent table and column ("" for a table without one).
	parent, column string
	// code: the table has a code column (countries).
	code bool
	// degree: the table has a degree column (departments).
	degree bool
	rows   func(*Dataset) *[]Row
}

// tables lists the reference tables, every parent before its children.
func tables() []table {
	return []table{
		{name: "countries", code: true, rows: func(d *Dataset) *[]Row { return &d.Countries }},
		{name: "cities", parent: "countries", column: "country_id", rows: func(d *Dataset) *[]Row { return &d.Cities }},
		{name: "districts", parent: "cities", column: "city_id", rows: func(d *Dataset) *[]Row { return &d.Districts }},
		{name: "universities", rows: func(d *Dataset) *[]Row { return &d.Universities }},
		{name: "faculties", parent: "universities", column: "university_id", rows: func(d *Dataset) *[]Row { return &d.Faculties }},
		{
			name: "departments", parent: "faculties", column: "faculty_id", degree: true,
			rows: func(d *Dataset) *[]Row { return &d.Departments },
		},
	}
}

// sourceCodes is the table of the departments' source codes. Its rows are
// keyed by the code, not by an id, so it is not one of tables(); Sync
// plans it after them under the same rules (sync.go).
const sourceCodes = "department_source_codes"

// sourceCodesHeader is the first line of department_source_codes.csv.
func sourceCodesHeader() []string { return []string{"code", "department_id"} }

// Tables lists the reference tables in the order Sync writes them.
func Tables() []string {
	ts := tables()
	out := make([]string, 0, len(ts)+1)
	for _, t := range ts {
		out = append(out, t.name)
	}
	return append(out, sourceCodes)
}

// header is the first line a table's file must have.
func (t table) header() []string {
	switch {
	case t.code:
		return []string{"id", "code", "name"}
	case t.degree:
		return []string{"id", t.column, "degree", "name"}
	case t.parent != "":
		return []string{"id", t.column, "name"}
	default:
		return []string{"id", "name"}
	}
}

// maxName is the names' length limit, the tables' CHECK.
const maxName = 200

var (
	countryCode = regexp.MustCompile(`^[A-Z]{2}$`)
	// programCode is an ÖSYM program code, the table's CHECK.
	programCode = regexp.MustCompile(`^\d{9}$`)
)

// Load reads and validates the embedded files.
func Load() (*Dataset, error) {
	raw, err := FS.ReadFile("VERSION")
	if err != nil {
		return nil, fmt.Errorf("referencedata: %w", err)
	}
	version, err := parseVersion(raw)
	if err != nil {
		return nil, err
	}
	d := Dataset{Version: version}
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
	data, err := FS.ReadFile(sourceCodes + ".csv")
	if err != nil {
		return nil, fmt.Errorf("referencedata: %w", err)
	}
	if d.DepartmentSourceCodes, err = parseSourceCodes(data); err != nil {
		return nil, err
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// maxVersion keeps the version in the integer column it is stored in.
const maxVersion = 1<<31 - 1

// parseVersion reads VERSION: one positive integer and a newline.
func parseVersion(raw []byte) (int, error) {
	v, err := strconv.Atoi(strings.TrimSuffix(string(raw), "\n"))
	if err != nil || v < 1 || v > maxVersion {
		return 0, fmt.Errorf("referencedata: VERSION %q: want one positive integer", raw)
	}
	return v, nil
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
		if t.degree {
			row.Degree = rec[2]
		}
		rows = append(rows, row)
	}
}

// parseSourceCodes reads department_source_codes.csv: an exact header,
// then one code and its department per record.
func parseSourceCodes(data []byte) ([]SourceCode, error) {
	r := csv.NewReader(bytes.NewReader(data))
	want := sourceCodesHeader()
	r.FieldsPerRecord = len(want)
	head, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("referencedata: %s.csv: header: %w", sourceCodes, err)
	}
	if !slices.Equal(head, want) {
		return nil, fmt.Errorf("referencedata: %s.csv: header %q, want %q", sourceCodes, head, want)
	}
	var codes []SourceCode
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return codes, nil
		}
		if err != nil {
			return nil, fmt.Errorf("referencedata: %s.csv: %w", sourceCodes, err)
		}
		department, err := uuid.Parse(rec[1])
		if err != nil {
			return nil, fmt.Errorf("referencedata: %s.csv: department_id %q: %w", sourceCodes, rec[1], err)
		}
		codes = append(codes, SourceCode{Code: rec[0], Department: department})
	}
}

// Validate checks what the database would refuse, and what it would take
// but should not, before anything is written and with the row named: the
// version is positive, ids are set and unique within a table, every row's
// parent is a row of the parent table, names are 1 to 200 characters with
// no control or invisible format character (Cf: U+200B, U+202E, U+FEFF
// ...), no line or paragraph separator (U+2028, U+2029) and no private use
// character (Co), and no surrounding space, and country codes are two capital letters
// and unique. A row whose parent the files drop has to be dropped too:
// retiring a university retires its faculties. A department's degree is
// ASSOCIATE or BACHELOR; a source code is nine digits, listed once, and
// names a department of the files. Validate does not look at
// the database; Sync adds the checks that need it (a moved row, the
// retirement limit, the version).
func (d *Dataset) Validate() error {
	if d.Version < 1 || d.Version > maxVersion {
		return fmt.Errorf("referencedata: version %d, want a positive version", d.Version)
	}
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
			switch {
			case t.degree && r.Degree != DegreeAssociate && r.Degree != DegreeBachelor:
				return fmt.Errorf("%s: degree %q, want %s or %s", where, r.Degree, DegreeAssociate, DegreeBachelor)
			case !t.degree && r.Degree != "":
				return fmt.Errorf("%s: %s has no degree", where, t.name)
			}
		}
		ids[t.name] = seen
	}
	codes := map[string]bool{}
	for _, c := range d.DepartmentSourceCodes {
		where := fmt.Sprintf("referencedata: %s %q", sourceCodes, c.Code)
		switch {
		case !programCode.MatchString(c.Code):
			return fmt.Errorf("%s: not an ÖSYM program code (nine digits)", where)
		case codes[c.Code]:
			return fmt.Errorf("%s: listed twice", where)
		case !ids["departments"][c.Department]:
			return fmt.Errorf("%s: department_id %s is not a row of departments", where, c.Department)
		}
		codes[c.Code] = true
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
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.Is(unicode.Cf, r) }):
		return errors.New("invisible format character (Cf)")
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.In(r, unicode.Zl, unicode.Zp) }):
		return errors.New("line or paragraph separator (Zl, Zp)")
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.Is(unicode.Co, r) }):
		return errors.New("private use character (Co)")
	}
	return nil
}

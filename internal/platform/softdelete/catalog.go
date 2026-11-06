package softdelete

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Class is the soft-delete class of a table: ClassSoftDelete, or one of
// the six exemption classes of ADR-0013 (M1–M6).
type Class string

// Classes of db/softdelete.yaml.
const (
	// ClassSoftDelete: deleted_at + deletion_id columns and an active_<t>
	// view; the default for a new table (K14).
	ClassSoftDelete Class = "soft_delete"
	// ClassLedger (M1): ledger and evidence; never deleted.
	ClassLedger Class = "M1"
	// ClassShortLived (M2): tokens and sessions; hard-deleted after a TTL.
	ClassShortLived Class = "M2"
	// ClassQueue (M3): River's tables; River manages them.
	ClassQueue Class = "M3"
	// ClassReference (M4): reference data; retired_at instead of deletion.
	ClassReference Class = "M4"
	// ClassDerived (M5): derived tables; rebuilt from their source.
	ClassDerived Class = "M5"
	// ClassLink (M6): link rows and singleton settings; hard delete or
	// upsert.
	ClassLink Class = "M6"
	// ClassTool: a table of a tool that manages it itself, such as goose's
	// migration history. No application role (koala_app, koala_maint)
	// holds any privilege on it (ADR-0020).
	ClassTool Class = "tool"
)

func (c Class) valid() bool {
	switch c {
	case ClassSoftDelete, ClassLedger, ClassShortLived, ClassQueue, ClassReference, ClassDerived, ClassLink, ClassTool:
		return true
	default:
		return false
	}
}

// Policy is what happens to a child row when its soft-deleted parent is
// deleted (ADR-0013 Karar 4).
type Policy string

// Edge policies.
const (
	// Cascade: the child is deleted with the parent, under the same
	// deletion_id. INNER JOIN on the edge is safe.
	Cascade Policy = "cascade"
	// Restrict: deleting a parent with an active child fails with 409
	// SOFT_DELETE_RESTRICTED.
	Restrict Policy = "restrict"
	// Detach: the child stays; the parent shows as a tombstone. Queries
	// reach the parent only with LEFT JOIN active_<parent> (CheckJoins).
	Detach Policy = "detach"
)

func (p Policy) valid() bool {
	return p == Cascade || p == Restrict || p == Detach
}

// Table is one table of the catalog.
type Table struct {
	Name  string
	Class Class
	// PII marks a table that holds personal data (KVKK). A soft-deleted
	// one's view is created WITH (security_barrier) (ADR-0017 §8); an
	// exempt one has no view and is marked for the retention and
	// anonymization inventory (ADR-0025).
	PII bool
	// Reason justifies an exemption; required for M1–M6 and tool.
	Reason string
	// UniqueExceptions maps a unique index of a soft-deleted table that is
	// not partial on deleted_at IS NULL to the reason (ADR-0013 Karar 5).
	UniqueExceptions map[string]string
	// AppView narrows koala_app's privileges on the active view below the
	// generated SELECT, INSERT, UPDATE (the migration revokes the rest),
	// for example a table only maintenance code inserts into. Empty means
	// the default.
	AppView []string
	// Retention maps a kind of row of an M1 table to how long it is kept,
	// as an ISO 8601 period of months or years ("P24M", "P10Y"). An M1
	// table with a retention is still never changed, but koala_maint
	// deletes its rows once the period has passed (ADR-0026); the
	// database computes each row's retain_until. Empty: never deleted.
	Retention map[string]string
}

// AppViewPrivileges returns koala_app's privileges on the active view, in
// SELECT, INSERT, UPDATE order.
func (t Table) AppViewPrivileges() []string {
	if len(t.AppView) == 0 {
		return []string{"SELECT", "INSERT", "UPDATE"}
	}
	var out []string
	for _, p := range []string{"SELECT", "INSERT", "UPDATE"} {
		if slices.Contains(t.AppView, p) {
			out = append(out, p)
		}
	}
	return out
}

// SoftDelete reports whether the table is soft-deleted (has a view).
func (t Table) SoftDelete() bool { return t.Class == ClassSoftDelete }

// View returns the name of the table's active view.
func (t Table) View() string { return ViewPrefix + t.Name }

// ViewPrefix starts the name of every active view.
const ViewPrefix = "active_"

// Edge is a foreign key to a soft-deleted table and its policy.
type Edge struct {
	// Child is the referencing table, Column its single FK column.
	Child, Column string
	// Parent is the soft-deleted table referenced (by its id).
	Parent string
	Policy Policy
}

// String formats the edge as child.column -> parent (policy).
func (e Edge) String() string {
	return fmt.Sprintf("%s.%s -> %s (%s)", e.Child, e.Column, e.Parent, e.Policy)
}

// Catalog is db/softdelete.yaml: the class of every table and the policy of
// every foreign key to a soft-deleted table. The catalog tests hold it
// equal to the database; softdelete.Delete follows its edges.
type Catalog struct {
	tables map[string]Table
	edges  []Edge
}

// Document shapes of the YAML file.
type (
	catalogDoc struct {
		Tables map[string]tableDoc `yaml:"tables"`
		Edges  []edgeDoc           `yaml:"edges"`
	}
	tableDoc struct {
		Class            Class             `yaml:"class"`
		PII              bool              `yaml:"pii"`
		Reason           string            `yaml:"reason"`
		UniqueExceptions map[string]string `yaml:"unique_exceptions"`
		AppView          []string          `yaml:"app_view"`
		Retention        map[string]string `yaml:"retention"`
	}
	edgeDoc struct {
		Child  string `yaml:"child"`
		Column string `yaml:"column"`
		Parent string `yaml:"parent"`
		Policy Policy `yaml:"policy"`
	}
)

// identifier is a plain lower-case Postgres identifier. Names from the
// catalog are written into SQL (quoted), so nothing else is accepted.
var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// retentionPeriod is an ISO 8601 period of whole months or years.
var retentionPeriod = regexp.MustCompile(`^P[1-9]\d*[MY]$`)

// MaxTableName is the longest name of a soft-deleted table: the longest
// name derived from it, <table>_deleted_together, must fit Postgres'
// 63-byte identifiers (longer ones are silently truncated). The catalog and
// softdeletegen apply the same limit.
const MaxTableName = 63 - len("_deleted_together")

// ParseCatalog parses one or more catalog documents into one catalog. Tests
// merge db/softdelete.yaml with the catalog of their example tables; a
// table listed twice is an error. Unknown keys are errors.
func ParseCatalog(docs ...[]byte) (*Catalog, error) {
	c := &Catalog{tables: map[string]Table{}}
	var edges []edgeDoc
	for i, data := range docs {
		doc, err := decode(data)
		if err != nil {
			return nil, fmt.Errorf("softdelete catalog %d: %w", i, err)
		}
		for name, td := range doc.Tables {
			if _, dup := c.tables[name]; dup {
				return nil, fmt.Errorf("softdelete catalog: table %s listed twice", name)
			}
			t := Table{Name: name, Class: td.Class, PII: td.PII, Reason: strings.TrimSpace(td.Reason), UniqueExceptions: td.UniqueExceptions, AppView: td.AppView, Retention: td.Retention}
			if err := t.validate(); err != nil {
				return nil, fmt.Errorf("softdelete catalog: table %s: %w", name, err)
			}
			c.tables[name] = t
		}
		edges = append(edges, doc.Edges...)
	}
	seen := map[string]bool{}
	for _, ed := range edges {
		e := Edge(ed)
		if err := c.validateEdge(e); err != nil {
			return nil, fmt.Errorf("softdelete catalog: edge %s.%s: %w", e.Child, e.Column, err)
		}
		key := e.Child + "." + e.Column
		if seen[key] {
			return nil, fmt.Errorf("softdelete catalog: edge %s listed twice", key)
		}
		seen[key] = true
		c.edges = append(c.edges, e)
	}
	slices.SortFunc(c.edges, func(a, b Edge) int {
		return strings.Compare(a.Child+"."+a.Column, b.Child+"."+b.Column)
	})
	return c, nil
}

// decode reads one catalog document strictly (go.yaml.in/yaml/v3,
// ADR-0020): an unknown key, a key given twice in one mapping, an empty
// input and a second document are errors.
func decode(data []byte) (catalogDoc, error) {
	var doc catalogDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return doc, errors.New("parse YAML: empty document")
		}
		return doc, fmt.Errorf("parse YAML: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return doc, errors.New("parse YAML: more than one document")
	}
	return doc, nil
}

func (t Table) validate() error {
	if !identifier.MatchString(t.Name) || len(t.Name) > MaxTableName {
		return fmt.Errorf("name must match %s and be at most %d bytes", identifier, MaxTableName)
	}
	if !t.Class.valid() {
		return fmt.Errorf("class %q: want soft_delete, M1–M6 or tool", t.Class)
	}
	if t.SoftDelete() {
		if strings.HasPrefix(t.Name, ViewPrefix) {
			return fmt.Errorf("a soft-deleted table cannot be named %s*", ViewPrefix)
		}
		for index, why := range t.UniqueExceptions {
			if !identifier.MatchString(index) || strings.TrimSpace(why) == "" {
				return fmt.Errorf("unique exception %q needs an index name and a reason", index)
			}
		}
		for _, p := range t.AppView {
			if p != "SELECT" && p != "INSERT" && p != "UPDATE" {
				return fmt.Errorf("app_view privilege %q: want SELECT, INSERT or UPDATE", p)
			}
		}
		if len(t.AppView) > 0 && !slices.Contains(t.AppView, "SELECT") {
			return errors.New("app_view must keep SELECT")
		}
		if len(t.Retention) > 0 {
			return errors.New("retention applies to M1 tables only; other classes state theirs in the reason")
		}
		return nil
	}
	switch {
	case t.Reason == "":
		return errors.New("an exempt table needs a reason")
	case len(t.AppView) > 0:
		return errors.New("app_view applies to soft-deleted tables (their views) only")
	case len(t.UniqueExceptions) > 0:
		return errors.New("unique_exceptions apply to soft-deleted tables only")
	case len(t.Retention) > 0 && t.Class != ClassLedger:
		return errors.New("retention applies to M1 tables only; other classes state theirs in the reason")
	}
	for kind, period := range t.Retention {
		if !identifier.MatchString(kind) || !retentionPeriod.MatchString(period) {
			return fmt.Errorf("retention %s: %q: want a lower_snake kind and a period such as P24M or P10Y", kind, period)
		}
	}
	return nil
}

func (c *Catalog) validateEdge(e Edge) error {
	if !identifier.MatchString(e.Column) {
		return fmt.Errorf("column must match %s", identifier)
	}
	child, ok := c.tables[e.Child]
	if !ok {
		return fmt.Errorf("child table %q is not in the catalog", e.Child)
	}
	parent, ok := c.tables[e.Parent]
	if !ok {
		return fmt.Errorf("parent table %q is not in the catalog", e.Parent)
	}
	if !parent.SoftDelete() {
		return fmt.Errorf("parent %s is not soft-deleted; only edges to soft-deleted tables need a policy", e.Parent)
	}
	if !e.Policy.valid() {
		return fmt.Errorf("policy %q: want cascade, restrict or detach", e.Policy)
	}
	if e.Policy == Cascade && !child.SoftDelete() {
		return fmt.Errorf("cascade needs a soft-deleted child; %s is %s", e.Child, child.Class)
	}
	return nil
}

// Table returns the named table.
func (c *Catalog) Table(name string) (Table, bool) {
	t, ok := c.tables[name]
	return t, ok
}

// Tables returns every table, sorted by name.
func (c *Catalog) Tables() []Table {
	names := slices.Sorted(maps.Keys(c.tables))
	out := make([]Table, len(names))
	for i, n := range names {
		out[i] = c.tables[n]
	}
	return out
}

// SoftDeleteTables returns the names of the soft-deleted tables, sorted.
func (c *Catalog) SoftDeleteTables() []string {
	var out []string
	for _, t := range c.Tables() {
		if t.SoftDelete() {
			out = append(out, t.Name)
		}
	}
	return out
}

// Edges returns every edge, sorted by child and column.
func (c *Catalog) Edges() []Edge { return slices.Clone(c.edges) }

// edgesFrom returns the edges whose parent is table.
func (c *Catalog) edgesFrom(table string) []Edge {
	var out []Edge
	for _, e := range c.edges {
		if e.Parent == table {
			out = append(out, e)
		}
	}
	return out
}

// relation is how application code reaches a table: the active view of a
// soft-deleted table, the table itself otherwise.
func (c *Catalog) relation(table string) string {
	if t := c.tables[table]; t.SoftDelete() {
		return t.View()
	}
	return table
}

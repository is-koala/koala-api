//go:build integration

package dbtest

import (
	"testing"

	"github.com/is-koala/koala-api/internal/platform/db"
)

// TestNewClonesTheNamedTemplate: the database New returns is a clone of
// the template templateOf names, the one whose name hashes the reference
// files and the sync (ADR-0037). TestTemplateNameCoversTheReferenceData
// binds what the name covers; this binds that New uses it: a New that
// named its template another way would leave no template of this name.
func TestNewClonesTheNamedTemplate(t *testing.T) {
	t.Parallel()

	d := New(t)
	_, name := templateOf(t)
	conn := connectAdmin(t.Context(), t)
	defer closeConn(t.Context(), conn)
	var template bool
	err := conn.QueryRow(t.Context(), "SELECT datistemplate FROM pg_database WHERE datname = $1", name).Scan(&template)
	if err != nil || !template {
		t.Fatalf("template %s: %v (template %v); New did not build or clone it", name, err, template)
	}
	// The clone carries the reference data the template was built with.
	var departments int
	owner := d.Open(t, db.RoleOwner)
	if err := owner.Pool().QueryRow(t.Context(), "SELECT count(*) FROM departments WHERE retired_at IS NULL").Scan(&departments); err != nil || departments == 0 {
		t.Fatalf("the clone has %d departments in use (%v)", departments, err)
	}
}

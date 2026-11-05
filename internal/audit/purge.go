package audit

import (
	"context"
	"fmt"

	"github.com/is-koala/koala-api/internal/platform/clock"
	"github.com/is-koala/koala-api/internal/platform/db"
	"github.com/is-koala/koala-api/internal/platform/db/maintdb"
)

// Purged counts the rows a purge deleted.
type Purged struct {
	Details, Entries, PIIAccess int64
}

// Purge deletes the audit trail's rows whose retention has passed: audit
// details (6 months), entries (24 months, FINANCIAL 10 years; result rows
// before their pending rows) and the PII access log (24 months). It is a
// maintenance task (`koala maint audit-purge`) and runs on a koala_maint
// pool: the evidence trigger refuses the delete to every other role and to
// rows still within their retention (ADR-0026). One transaction.
func Purge(ctx context.Context, maint *db.DB, clk clock.Clock) (Purged, error) {
	before := clk.Now()
	var p Purged
	err := maint.InTx(ctx, func(ctx context.Context, q db.Querier) error {
		m := maintdb.New(q)
		var err error
		if p.Details, err = m.PurgeAuditDetails(ctx, before); err != nil {
			return fmt.Errorf("audit: purge details: %w", err)
		}
		results, err := m.PurgeAuditResults(ctx, before)
		if err != nil {
			return fmt.Errorf("audit: purge result rows: %w", err)
		}
		rest, err := m.PurgeAuditEntries(ctx, before)
		if err != nil {
			return fmt.Errorf("audit: purge entries: %w", err)
		}
		p.Entries = results + rest
		if p.PIIAccess, err = m.PurgePIIAccessLog(ctx, before); err != nil {
			return fmt.Errorf("audit: purge the PII access log: %w", err)
		}
		return nil
	})
	if err != nil {
		return Purged{}, fmt.Errorf("audit: purge: %w", err)
	}
	return p, nil
}

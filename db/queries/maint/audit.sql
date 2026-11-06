-- name: PurgeAuditDetails :execrows
-- PurgeAuditDetails deletes audit details past their retention (6 months).
-- The trigger forbid_evidence_change allows the delete to koala_maint only
-- once retain_until has passed by the database's clock too.
DELETE FROM admin_audit_entry_details
WHERE retain_until <= least(@before::timestamptz, now());

-- name: PurgeAuditResults :execrows
-- PurgeAuditResults deletes the result rows of mutations past their
-- retention, before their pending rows (result_of references them).
DELETE FROM admin_audit_entries
WHERE result_of IS NOT NULL AND retain_until <= least(@before::timestamptz, now());

-- name: PurgeAuditEntries :execrows
-- PurgeAuditEntries deletes the remaining entries past their retention:
-- refusals, reads and pending rows whose result is gone.
DELETE FROM admin_audit_entries e
WHERE e.retain_until <= least(@before::timestamptz, now())
  AND NOT EXISTS (SELECT FROM admin_audit_entries r WHERE r.result_of = e.id)
  AND NOT EXISTS (SELECT FROM admin_audit_entry_details d WHERE d.entry_id = e.id);

-- name: PurgePIIAccessLog :execrows
-- PurgePIIAccessLog deletes PII access rows past their retention (24 months).
DELETE FROM pii_access_log
WHERE retain_until <= least(@before::timestamptz, now());

#!/usr/bin/env bash
# Proves that sqlc.yaml's vet rules fire (ADR-0020). no-base-table is
# generated from the soft-delete catalog; the real catalog may have no
# soft-deleted table yet, so the probe builds both rules from the real
# catalog merged with the example tables of the test databases, with the
# same generator (softdeletegen -print-rule), and runs `sqlc vet` on
# generated queries:
#   - application queries that name a base table fail no-base-table, and
#     those that can write deleted_at or deletion_id fail
#     no-soft-delete-columns: exactly one finding each. The forms of
#     ADR-0027 Karar 1 are here except MERGE and the row-constructor SET,
#     which the probe cannot carry (it accepts no compile error and neither
#     is a proven sqlc path); those two are covered by
#     TestNoSoftDeleteColumnsPatterns alone;
#   - application queries through active_ views, and on M1 tables, pass;
#   - the same queries in the maint package pass: the rules are not applied
#     there.
# The probe files are always removed; they are never committed.
# Usage: scripts/sqlc-vet-probe.sh <sqlc binary>
set -euo pipefail

sqlc="$1"
dir=zzsqlcprobe
trap 'rm -rf "$dir"' EXIT
rm -rf "$dir"
mkdir -p "$dir/app" "$dir/maint"

catalogs="db/softdelete.yaml internal/platform/dbtest/testdata/softdelete.yaml"
# shellcheck disable=SC2086 # two catalog paths
base_rule="$(go run ./internal/tools/softdeletegen -print-rule no-base-table $catalogs)"
# shellcheck disable=SC2086
columns_rule="$(go run ./internal/tools/softdeletegen -print-rule no-soft-delete-columns $catalogs)"
if [ "$base_rule" = "false" ]; then
	echo "sqlc-vet-probe: the merged catalog has no soft-deleted table" >&2
	exit 1
fi

cat > "$dir/sqlc.yaml" <<EOF
version: "2"
sql:
  - name: app
    engine: postgresql
    schema: [../db/migrations, ../internal/platform/dbtest/testdata/migrations]
    queries: app
    rules: [no-base-table, no-soft-delete-columns]
  - name: maint
    engine: postgresql
    schema: [../db/migrations, ../internal/platform/dbtest/testdata/migrations]
    queries: maint
rules:
  - name: no-base-table
    message: probe
    rule: '${base_rule//\'/\'\'}'
  - name: no-soft-delete-columns
    message: probe
    rule: '${columns_rule//\'/\'\'}'
EOF

cat > "$dir/app/bad.sql" <<'EOF'
-- name: BadSelect :many
SELECT id FROM example_owners;

-- name: BadQualified :many
SELECT id FROM public.example_owners;

-- name: BadJoin :many
SELECT p.id FROM active_example_projects p JOIN example_owners o ON o.id = p.owner_id;

-- name: BadInsert :exec
INSERT INTO example_notes (id, body) VALUES ($1, $2);

-- name: BadUpdate :exec
UPDATE example_tasks SET name = $2 WHERE id = $1;

-- name: BadDelete :exec
DELETE FROM example_comments WHERE id = $1;

-- name: BadUpperCase :many
SELECT ID FROM EXAMPLE_PROJECTS;

-- name: BadUsing :exec
DELETE FROM active_example_notes n USING example_owners o WHERE o.id = n.id;

-- name: BadSetDeletedAt :exec
UPDATE active_example_tasks SET deleted_at = now() WHERE id = $1;

-- name: BadSetQuotedDeletionID :exec
UPDATE active_example_tasks SET name = $2, "deletion_id" = $3 WHERE id = $1;

-- name: BadInsertDeletedAt :exec
INSERT INTO active_example_notes (id, body, deleted_at) VALUES ($1, $2, $3);

-- name: BadInsertWithoutColumns :exec
INSERT INTO active_example_notes VALUES ($1, $2, $3, $4, $5);

-- name: BadInsertSelectWithoutColumns :exec
INSERT INTO active_example_notes SELECT * FROM active_example_notes WHERE id = $1;

-- name: BadSetSplitByAComment :exec
UPDATE active_example_tasks SET deleted_at /* no way around the rule */ = now() WHERE id = $1;
EOF

cat > "$dir/app/good.sql" <<'EOF'
-- name: GoodSelect :many
SELECT id, name, deleted_at FROM active_example_owners;

-- name: GoodLeftJoin :many
SELECT c.id, o.name FROM active_example_comments c LEFT JOIN active_example_owners o ON o.id = c.author_id;

-- name: GoodUpdate :exec
UPDATE active_example_tasks SET name = $2 WHERE id = $1;

-- name: GoodLedger :exec
INSERT INTO deletions (id, root_table, root_id, actor_kind, reason, created_at) VALUES ($1, $2, $3, 'system', 'retention', $4);
EOF

cat > "$dir/maint/maint.sql" <<'EOF'
-- name: MaintSelect :many
SELECT id FROM example_owners WHERE deletion_id = $1;

-- name: MaintRestore :exec
UPDATE example_tasks SET deleted_at = NULL, deletion_id = NULL WHERE deletion_id = $1;

-- name: MaintInsert :exec
INSERT INTO example_notes (id, body, deleted_at, deletion_id) VALUES ($1, $2, $3, $4);
EOF

if out="$("$sqlc" vet -f "$dir/sqlc.yaml" 2>&1)"; then
	echo "sqlc-vet-probe: sqlc vet reported nothing; the rule is not active" >&2
	exit 1
fi

want="bad.sql: BadSelect: no-base-table: probe
bad.sql: BadQualified: no-base-table: probe
bad.sql: BadJoin: no-base-table: probe
bad.sql: BadInsert: no-base-table: probe
bad.sql: BadUpdate: no-base-table: probe
bad.sql: BadDelete: no-base-table: probe
bad.sql: BadUpperCase: no-base-table: probe
bad.sql: BadUsing: no-base-table: probe
bad.sql: BadSetDeletedAt: no-soft-delete-columns: probe
bad.sql: BadSetQuotedDeletionID: no-soft-delete-columns: probe
bad.sql: BadInsertDeletedAt: no-soft-delete-columns: probe
bad.sql: BadInsertWithoutColumns: no-soft-delete-columns: probe
bad.sql: BadInsertSelectWithoutColumns: no-soft-delete-columns: probe
bad.sql: BadSetSplitByAComment: no-soft-delete-columns: probe"

# The output must be exactly the expected findings: no finding on the good
# or maint queries, no second rule on a bad query, and no compile error (the
# views must be in sqlc's catalog).
status=0
if [ "$(sort <<<"$out")" != "$(sort <<<"$want")" ]; then
	echo "sqlc-vet-probe: findings differ from the expected ones" >&2
	diff <(sort <<<"$want") <(sort <<<"$out") >&2 || true
	status=1
fi
if [ "$status" -ne 0 ]; then
	echo "$out" >&2
	exit 1
fi
echo "sqlc-vet-probe: ok"

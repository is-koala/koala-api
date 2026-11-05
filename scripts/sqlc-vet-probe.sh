#!/usr/bin/env bash
# Proves that sqlc.yaml's no-base-table rule fires (ADR-0020). The rule is
# generated from the soft-delete catalog; the real catalog may have no
# soft-deleted table yet, so the probe builds the rule from the real catalog
# merged with the example tables of the test databases, with the same
# generator (softdeletegen -print-rule), and runs `sqlc vet` on generated
# queries:
#   - application queries that name a base table fail, one finding each;
#   - application queries through active_ views, and on M1 tables, pass;
#   - the same base-table queries in the maint package pass: the rule is
#     not applied there.
# The probe files are always removed; they are never committed.
# Usage: scripts/sqlc-vet-probe.sh <sqlc binary>
set -euo pipefail

sqlc="$1"
dir=zzsqlcprobe
trap 'rm -rf "$dir"' EXIT
rm -rf "$dir"
mkdir -p "$dir/app" "$dir/maint"

rule="$(go run ./internal/tools/softdeletegen -print-rule db/softdelete.yaml internal/platform/dbtest/testdata/softdelete.yaml)"
if [ "$rule" = "false" ]; then
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
    rules: [no-base-table]
  - name: maint
    engine: postgresql
    schema: [../db/migrations, ../internal/platform/dbtest/testdata/migrations]
    queries: maint
rules:
  - name: no-base-table
    message: probe
    rule: '${rule//\'/\'\'}'
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
EOF

if out="$("$sqlc" vet -f "$dir/sqlc.yaml" 2>&1)"; then
	echo "sqlc-vet-probe: sqlc vet reported nothing; the rule is not active" >&2
	exit 1
fi

status=0
for q in BadSelect BadQualified BadJoin BadInsert BadUpdate BadDelete BadUpperCase; do
	if ! grep -qF -- "bad.sql: $q: no-base-table: probe" <<<"$out"; then
		echo "sqlc-vet-probe: missing expected finding for $q" >&2
		status=1
	fi
done
# Every line must be one of the expected findings: no finding on the good
# or maint queries, and no compile error (the views must be in sqlc's
# catalog).
if grep -vE '^bad\.sql: Bad[A-Za-z]+: no-base-table: probe$' <<<"$out" | grep -q .; then
	echo "sqlc-vet-probe: unexpected output" >&2
	status=1
fi
if [ "$status" -ne 0 ]; then
	echo "$out" >&2
	exit 1
fi
echo "sqlc-vet-probe: ok"

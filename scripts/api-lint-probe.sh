#!/usr/bin/env bash
# Proves that every koala-* rule in api/ruleset.yaml and
# api/ruleset-schemas.yaml fires: api/testdata/lint-probe.yaml breaks each
# rule at least once. A rule whose JSONPath or function silently matches
# nothing would otherwise pass every spec. Usage:
# scripts/api-lint-probe.sh <vacuum binary>
set -euo pipefail

vacuum="$1"
out="$(mktemp)"
trap 'rm -f "$out"' EXIT

set +e
"$vacuum" lint --no-update-check --no-banner --no-style --details --all-results --no-clip \
  --ruleset api/ruleset.yaml api/testdata/lint-probe.yaml >"$out" 2>&1
code=$?
set -e
# vacuum exits 1 for violations and 2 for a tool or input error.
if [ "$code" -ne 1 ]; then
  cat "$out"
  echo "api lint probe: vacuum exited $code, want 1 (violations found)"
  exit 1
fi

status=0
for rule in $(grep -ho '^  koala-[a-z0-9-]*' api/ruleset.yaml api/ruleset-schemas.yaml); do
  if ! grep -q "rule: ${rule} " "$out"; then
    echo "api lint probe: rule ${rule} did not fire on api/testdata/lint-probe.yaml"
    status=1
  fi
done
exit "$status"

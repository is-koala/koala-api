#!/usr/bin/env bash
# Proves that the koala-* rules in api/ruleset.yaml and
# api/ruleset-schemas.yaml catch exactly what they should (ADR-0019):
#   - api/testdata/lint-probe.yaml breaks each rule in independent cases;
#     its koala-* results must equal api/testdata/lint-probe.expect, one
#     (rule, JSON path) pair per line. A missing pair is a rule that stopped
#     catching its case; an extra one is a false positive or an unlisted case.
#   - api/testdata/lint-pass.yaml holds the correct forms; it must report no
#     koala-* result.
#   - every koala-* rule has at least one expected pair.
# Usage: scripts/api-lint-probe.sh <vacuum binary> [per-rule timeout in seconds]
set -euo pipefail

vacuum="$1"
# vacuum skips a rule that runs out of its per-rule budget, which would
# turn a slow rule into a silently passing one; the Makefile passes the
# same budget it uses for the contract (ADR-0029).
timeout="${2:-60}"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

# koala_results <spec> <out>: the sorted koala-* (rule, path) pairs vacuum
# reports for spec. vacuum exits 1 for violations and 2 for a tool or
# input error; 2 fails the probe.
koala_results() {
	set +e
	"$vacuum" lint --no-update-check --no-banner --no-style --details --all-results --no-clip \
		--timeout "$timeout" --ruleset api/ruleset.yaml "$1" >"$dir/raw" 2>&1
	local code=$?
	set -e
	if [ "$code" -gt 1 ]; then
		cat "$dir/raw"
		echo "api lint probe: vacuum exited $code on $1"
		exit 1
	fi
	awk '/^\$/ {path = $0} /^rule: koala-/ {print $2 "\t" path}' "$dir/raw" | sort -u >"$2"
}

status=0

koala_results api/testdata/lint-probe.yaml "$dir/got"
grep -v '^#' api/testdata/lint-probe.expect | sort -u >"$dir/want"
if ! diff -u "$dir/want" "$dir/got" >"$dir/diff"; then
	echo "api lint probe: lint-probe.yaml results differ from lint-probe.expect (- missing, + unexpected):"
	grep -E '^[-+][^-+]' "$dir/diff"
	status=1
fi

koala_results api/testdata/lint-pass.yaml "$dir/pass"
if [ -s "$dir/pass" ]; then
	echo "api lint probe: lint-pass.yaml must report nothing, got:"
	cat "$dir/pass"
	status=1
fi

for rule in $(grep -ho '^  koala-[a-z0-9-]*' api/ruleset.yaml api/ruleset-schemas.yaml); do
	if ! cut -f1 "$dir/want" | grep -qx "$rule"; then
		echo "api lint probe: rule $rule has no case in lint-probe.expect"
		status=1
	fi
done

# The style guide marks each of its rules with [lint: koala-…]. vacuum
# reads the ruleset, never the document, so a mark left behind by a
# renamed or dropped rule would go unnoticed: every name the guide marks
# must name a rule that exists.
grep -ho '^  koala-[a-z0-9-]*' api/ruleset.yaml api/ruleset-schemas.yaml | tr -d ' ' | sort -u >"$dir/rules"
for name in $(grep -o '\[lint: [^]]*\]' docs/api-style-guide.md | grep -o 'koala-[a-z0-9-]*' | sort -u); do
	if ! grep -qx "$name" "$dir/rules"; then
		echo "api lint probe: docs/api-style-guide.md marks $name, which api/ruleset*.yaml does not define"
		status=1
	fi
done
exit "$status"

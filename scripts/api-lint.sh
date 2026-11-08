#!/usr/bin/env bash
# Lints api/ with vacuum and refuses any run in which a rule did not finish.
#
# vacuum gives every rule a wall-clock budget per file (--timeout) and logs
#
#	ERR Rule timed out, skipping
#	  ├──  rule: koala-operation-authz
#	  └──  timeout: 5s
#
# when a rule runs out of it. A skipped rule reports nothing, and a rule
# that reports nothing is indistinguishable from a rule that passed: the
# protection is present, does nothing, and the gate is green. That is this
# repository's dominant defect class, in the gate itself (ADR-0029).
#
# vacuum v0.36.2 also refuses the file after a timeout ("Unable to process
# spec", exit 2), so the gate does break today. We do not rely on that:
# the wording "skipping" is the risk, the behaviour is the tool's choice,
# and the budget will be outgrown again as the contract grows towards the
# 266 endpoints of plan §1.2. So the skip is detected in the output as
# well, and one skipped rule fails this step whatever vacuum's exit code
# says.
#
# Usage: scripts/api-lint.sh <vacuum binary> <per-rule timeout in seconds>
set -euo pipefail

vacuum="$1"
timeout="$2"

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

# unfinished is the signature of a rule vacuum did not run to completion.
# It is matched against vacuum's own words, not against a rule name: no
# koala-* rule description contains either phrase (the probe's rule walk
# would notice if one started to).
unfinished='Rule timed out, skipping|timed out after'

# lint <ruleset> <spec>: lint spec and fail on a violation or on a rule
# that did not finish.
lint() {
	local ruleset="$1" spec="$2" code=0
	"$vacuum" lint --no-update-check --no-banner --no-style --details --no-clip \
		--timeout "$timeout" --ruleset "$ruleset" "$spec" >"$log" 2>&1 || code=$?
	cat "$log"
	if grep -qaE "$unfinished" "$log"; then
		echo
		echo "api lint: vacuum did not finish every rule on $spec (per-rule budget ${timeout}s)."
		echo "A skipped rule reports nothing, so it is indistinguishable from a passing rule."
		echo "Raise VACUUM_TIMEOUT in the Makefile, or make the rule cheaper; do not ignore this."
		return 1
	fi
	return "$code"
}

status=0
lint api/ruleset.yaml api/openapi.yaml || status=1
for f in api/components/*.yaml; do
	echo "vacuum lint --ruleset api/ruleset-schemas.yaml $f"
	lint api/ruleset-schemas.yaml "$f" || status=1
done
exit "$status"

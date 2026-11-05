#!/usr/bin/env bash
# Runs the integration tests (build tag `integration`) and only them: the
# unit tests already ran in `make verify`. Needs Postgres through the libpq
# environment (PGHOST, PGPORT, PGUSER, ...); in CI that is the service
# container of verify.yml (ADR-0017). Not for the shared Mac.
#
# Before running, it guards against an integration test that silently
# never runs:
#   - every *_integration_test.go starts with exactly `//go:build integration`;
#   - no other test file has a build constraint (a misspelled tag would
#     hide the file from both runs);
#   - a test file that uses dbtest is an *_integration_test.go file.
set -euo pipefail

files=$(git ls-files '*_integration_test.go')
if [ -z "$files" ]; then
	echo "test-integration: no *_integration_test.go files" >&2
	exit 1
fi

status=0
for f in $files; do
	if [ "$(head -n 1 "$f")" != "//go:build integration" ]; then
		echo "test-integration: $f: first line must be //go:build integration" >&2
		status=1
	fi
done
while IFS= read -r f; do
	case "$f" in
	*_integration_test.go) ;;
	*)
		echo "test-integration: $f: build constraint in a test file that is not *_integration_test.go" >&2
		status=1
		;;
	esac
done < <(git grep -l -E '^//go:build' -- '*_test.go' || true)
while IFS= read -r f; do
	case "$f" in
	*_integration_test.go) ;;
	*)
		echo "test-integration: $f: uses dbtest but is not an *_integration_test.go file" >&2
		status=1
		;;
	esac
done < <(git grep -l 'internal/platform/dbtest"' -- '*_test.go' || true)
[ "$status" -eq 0 ] || exit 1

pkgs=$(for f in $files; do echo "./$(dirname "$f")"; done | sort -u)
tests=$(grep -h -o -E '^func Test[A-Za-z0-9_]+' $files | sed 's/^func //' | sort -u | paste -s -d '|' -)
if [ -z "$tests" ]; then
	echo "test-integration: no Test functions in the integration test files" >&2
	exit 1
fi

# shellcheck disable=SC2086 # pkgs is a list of package paths
exec go test -tags integration -race -shuffle=on -count=1 -run "^(${tests})\$" $pkgs

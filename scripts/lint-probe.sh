#!/usr/bin/env bash
# Proves that the depguard/forbidigo/sloglint rules in .golangci.yml fire
# (and that depguard's platform allow/deny overlap resolves as intended),
# including the maint rule that keeps koala_maint's queries (maintdb) out of
# application code.
# The probe packages are generated here, linted, and always removed; they
# are never committed. Usage: scripts/lint-probe.sh <golangci-lint binary>
set -euo pipefail

lint="$1"
domain_dir=internal/zzlintprobe
platform_dir=internal/platform/zzlintprobe
trap 'rm -rf "$domain_dir" "$platform_dir"' EXIT
mkdir -p "$domain_dir" "$platform_dir"

cat > "$domain_dir/domain.go" <<'EOF'
// Package zzlintprobe is a generated lint probe.
package zzlintprobe

import (
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/is-koala/koala-api/internal/platform/db/maintdb"
)

// F is a probe.
func F() time.Time {
	_ = maintdb.New
	log.Println(http.MethodGet)
	slog.Info("probe")
	return time.Now()
}
EOF

cat > "$platform_dir/probe.go" <<'EOF'
// Package zzlintprobe is a generated lint probe.
package zzlintprobe

import (
	"github.com/is-koala/koala-api/internal/app"
	"github.com/is-koala/koala-api/internal/platform/apperr"
)

// G is a probe.
func G() (int, apperr.Kind) { return app.ExitOK, apperr.KindInternal }
EOF

if out="$("$lint" run ./"$domain_dir"/... ./"$platform_dir"/... 2>&1)"; then
	echo "lint-probe: golangci-lint reported nothing; the rules are not active" >&2
	exit 1
fi

status=0
for want in \
	"internal/app' is not allowed from list 'platform'" \
	"import 'log' is not allowed from list 'rejected'" \
	"import 'net/http' is not allowed from list 'domain'" \
	"internal/platform/db/maintdb' is not allowed from list 'maint'" \
	"use of \`time.Now\` forbidden" \
	"(sloglint)"; do
	if ! grep -qF -- "$want" <<<"$out"; then
		echo "lint-probe: missing expected finding: $want" >&2
		status=1
	fi
done
# platform may import platform: the allow entry must win over the deny prefix.
for unwanted in \
	"internal/platform/apperr' is not allowed" \
	"import 'log/slog' is not allowed"; do
	if grep -qF -- "$unwanted" <<<"$out"; then
		echo "lint-probe: unexpected finding: $unwanted" >&2
		status=1
	fi
done
if [ "$status" -ne 0 ]; then
	echo "$out" >&2
	exit 1
fi
echo "lint-probe: ok"

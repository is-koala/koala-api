#!/usr/bin/env bash
# Database step of a release (ADR-0017). Runs before the new server image
# takes traffic, with the image being released:
#
#   scripts/release-db.sh ghcr.io/is-koala/koala-api:v1.2.3
#
# 1. koala bootstrap   creates the group roles if missing (idempotent)
# 2. koala migrate up  applies pending migrations as koala_owner
#
# Each step stops the release on failure. Variables are passed to the
# container by name only (docker run -e NAME), so no value appears on the
# command line: KOALA_ENV, KOALA_BOOTSTRAP_DATABASE_URL (non-superuser,
# CREATEROLE, to the postgres database), KOALA_OWNER_DATABASE_URL (a member
# of koala_owner). The
# runbook (docs/runbook/veritabani.md) explains the logins.
set -euo pipefail

image="${1:?usage: scripts/release-db.sh <image>}"
for name in KOALA_ENV KOALA_BOOTSTRAP_DATABASE_URL KOALA_OWNER_DATABASE_URL; do
	if [ -z "${!name:-}" ]; then
		echo "release-db: $name is not set" >&2
		exit 1
	fi
done

run() {
	docker run --rm --network host \
		-e KOALA_ENV -e KOALA_BOOTSTRAP_DATABASE_URL -e KOALA_OWNER_DATABASE_URL \
		"$image" "$@"
}

run bootstrap
run migrate up

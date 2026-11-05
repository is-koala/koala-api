#!/usr/bin/env bash
# Builds, smoke-tests and publishes the koala image (ADR-0023). CI only:
# the verify workflow's image job and the release workflow call it; images
# are never built on the shared Mac (ADR-0004).
#
#   scripts/image.sh build   <version> <ref>              build into the local Docker
#   scripts/image.sh smoke   <ref> <version>              run the image and check it
#   scripts/image.sh publish <repo> <version> [--latest]  push, check, write SBOM + provenance
#
# All three build the same thing: linux/amd64, VERSION=<version>, the same
# labels. publish runs after build in the same job, so its build is a
# cache hit of the image smoke just tested.
#
# The pushed tag is a single linux/amd64 image manifest, not an index:
# Docker Swarm (Dokploy) cannot schedule an index that carries attestation
# manifests. So the registry gets no attestations; publish writes the SBOM
# (SPDX) and the SLSA provenance that BuildKit generates as files instead,
# with the pushed digest, into $ATTEST_DIR (default dist/attest).
set -euo pipefail

# BuildKit's SBOM generator, pinned like the base images (Docker's own scanner).
SBOM_GENERATOR="docker/buildkit-syft-scanner:1.12.0@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9"
SOURCE_URL="https://github.com/is-koala/koala-api"
# Plan §6.1, Faz 1 exit criterion: startup under 1 s.
MAX_STARTUP_MS="${SMOKE_MAX_STARTUP_MS:-1000}"

fail() {
	echo "::error::image: $*" >&2
	exit 1
}

now_ms() { echo $(($(date +%s%N) / 1000000)); }

# Flags every build shares, so that the smoke-tested image and the pushed
# one come from the same cache entries.
common_args() {
	local version="$1"
	local revision="${GITHUB_SHA:-$(git rev-parse HEAD)}"
	printf '%s\n' \
		--platform linux/amd64 \
		--build-arg "VERSION=$version" \
		--label "org.opencontainers.image.source=$SOURCE_URL" \
		--label "org.opencontainers.image.revision=$revision" \
		--label "org.opencontainers.image.version=$version"
}

# Every FROM line must pin its image by digest (ADR-0004).
check_pins() {
	local unpinned
	unpinned="$(grep -E '^FROM ' Dockerfile | grep -v '@sha256:[0-9a-f]\{64\}' || true)"
	[ -z "$unpinned" ] || fail "Dockerfile base image not pinned by digest: $unpinned"
}

cmd_build() {
	local version="${1:?usage: scripts/image.sh build <version> <ref>}"
	local ref="${2:?usage: scripts/image.sh build <version> <ref>}"
	check_pins
	local args
	mapfile -t args < <(common_args "$version")
	docker buildx build "${args[@]}" --provenance=false --sbom=false --load -t "$ref" .
}

# --- smoke -------------------------------------------------------------------

smoke_container=koala-smoke

smoke_cleanup() {
	docker rm -f -v "$smoke_container" >/dev/null 2>&1 || true
}

smoke_logs_and_fail() {
	docker logs "$smoke_container" 2>&1 | tail -n 50 >&2 || true
	fail "$1"
}

psql_admin() {
	psql -X -q -v ON_ERROR_STOP=1 -d postgres -c "$1"
}

# Polls url until it answers 200 or timeout_ms passes; prints the time of
# the first 200 in ms since the epoch.
wait_200() {
	local url="$1" timeout_ms="$2" start
	start="$(now_ms)"
	until curl -fsS -o /dev/null --max-time 1 "$url" 2>/dev/null; do
		if [ $(($(now_ms) - start)) -gt "$timeout_ms" ]; then
			return 1
		fi
		sleep 0.005
	done
	now_ms
}

# Needs Postgres through PGHOST/PGPORT/PGUSER as a role that may create
# logins and databases: the job's service container (trust auth, so no
# password exists). The logins below exist only in that container.
cmd_smoke() {
	local image="${1:?usage: scripts/image.sh smoke <ref> <version>}"
	local version="${2:?usage: scripts/image.sh smoke <ref> <version>}"
	: "${PGHOST:?PGHOST is required}" "${PGPORT:?PGPORT is required}" "${PGUSER:?PGUSER is required}"
	trap smoke_cleanup EXIT
	smoke_cleanup

	# 1. Distroless and nonroot.
	local user
	user="$(docker image inspect -f '{{.Config.User}}' "$image")"
	[ "$user" = "nonroot:nonroot" ] || fail "image user is '$user', want nonroot:nonroot"
	if docker run --rm --entrypoint /bin/sh "$image" -c true >/dev/null 2>&1; then
		fail "image has a shell; want distroless"
	fi
	local got
	got="$(docker run --rm "$image" version)"
	[ "$got" = "$version" ] || fail "koala version printed '$got', want '$version'"

	# 2. The release's database step with this image (ADR-0017): the first
	# bootstrap creates the group roles; the operator then creates the
	# logins and the database; scripts/release-db.sh runs bootstrap again
	# (idempotent) and migrate up.
	local base="postgres://"
	local hostport="${PGHOST}:${PGPORT}"
	export KOALA_ENV=test
	export KOALA_BOOTSTRAP_DATABASE_URL="${base}koala_smoke_bootstrap@${hostport}/postgres?sslmode=disable"
	export KOALA_OWNER_DATABASE_URL="${base}koala_smoke_owner@${hostport}/koala_smoke?sslmode=disable"
	export KOALA_DATABASE_URL="${base}koala_smoke_app@${hostport}/koala_smoke?sslmode=disable"
	psql_admin "CREATE ROLE koala_smoke_bootstrap LOGIN CREATEROLE"
	docker run --rm --network host -e KOALA_ENV -e KOALA_BOOTSTRAP_DATABASE_URL "$image" bootstrap
	psql_admin "CREATE ROLE koala_smoke_owner LOGIN IN ROLE koala_owner"
	psql_admin "CREATE ROLE koala_smoke_app LOGIN IN ROLE koala_app"
	psql_admin "CREATE DATABASE koala_smoke OWNER koala_owner"
	scripts/release-db.sh "$image"

	# 3. Startup. Two clocks: from `docker start` (includes the runtime's
	# own overhead) and from the container's StartedAt (the process). The
	# gate is the process time; both are reported.
	docker create --name "$smoke_container" --network host \
		-e KOALA_ENV -e KOALA_DATABASE_URL "$image" >/dev/null
	local t_start t_live t_ready
	t_start="$(now_ms)"
	docker start "$smoke_container" >/dev/null
	t_live="$(wait_200 http://127.0.0.1:8080/livez 10000)" ||
		smoke_logs_and_fail "/livez did not answer 200 within 10 s"
	t_ready="$(wait_200 http://127.0.0.1:8080/readyz 10000)" ||
		smoke_logs_and_fail "/readyz did not turn 200 within 10 s"
	local started_at t_proc
	started_at="$(docker inspect -f '{{.State.StartedAt}}' "$smoke_container")"
	t_proc=$(($(date -d "$started_at" +%s%N) / 1000000))
	local livez_ms=$((t_live - t_proc)) livez_docker_ms=$((t_live - t_start)) readyz_ms=$((t_ready - t_proc))
	echo "startup: /livez ${livez_ms} ms after process start (${livez_docker_ms} ms after docker start), /readyz ${readyz_ms} ms"
	[ "$livez_ms" -lt "$MAX_STARTUP_MS" ] ||
		smoke_logs_and_fail "startup took ${livez_ms} ms, want < ${MAX_STARTUP_MS} ms"

	# The running process, not only the image config, is nonroot (65532).
	local pid uid
	pid="$(docker inspect -f '{{.State.Pid}}' "$smoke_container")"
	uid="$(ps -o uid= -p "$pid" | tr -d ' ')"
	[ "$uid" = 65532 ] || smoke_logs_and_fail "server runs as uid $uid, want 65532 (nonroot)"

	# 4. koala healthcheck: 0 against the server, 1 without one; Docker's
	# HEALTHCHECK (the same command) turns healthy.
	docker exec "$smoke_container" /koala healthcheck ||
		smoke_logs_and_fail "koala healthcheck failed against a running server"
	if docker run --rm --network none -e KOALA_ENV=test "$image" healthcheck 2>/dev/null; then
		fail "koala healthcheck succeeded with no server"
	fi
	local health="" i
	for i in $(seq 1 40); do
		health="$(docker inspect -f '{{.State.Health.Status}}' "$smoke_container")"
		[ "$health" = healthy ] && break
		sleep 1
	done
	[ "$health" = healthy ] || smoke_logs_and_fail "Docker HEALTHCHECK is '$health' after 40 s, want healthy"

	# 5. SIGTERM: readiness 503, drain, exit 0 (plan §3.10). 45 s is the
	# stop_grace_period the runbook asks for.
	local t_stop exit_code stop_ms
	t_stop="$(now_ms)"
	docker stop -t 45 "$smoke_container" >/dev/null
	stop_ms=$(($(now_ms) - t_stop))
	exit_code="$(docker inspect -f '{{.State.ExitCode}}' "$smoke_container")"
	[ "$exit_code" = 0 ] || smoke_logs_and_fail "exit code after SIGTERM is $exit_code, want 0"
	docker logs "$smoke_container" 2>&1 | grep -q '"msg":"stopped"' ||
		smoke_logs_and_fail "no 'stopped' log line after SIGTERM"

	# Size: uncompressed (as unpacked on the host) and gzip (about what the
	# registry stores).
	local size_bytes gzip_bytes size_mb gzip_mb
	size_bytes="$(docker image inspect -f '{{.Size}}' "$image")"
	gzip_bytes="$(docker save "$image" | gzip -c | wc -c)"
	size_mb="$(awk -v b="$size_bytes" 'BEGIN { printf "%.1f", b / 1000000 }')"
	gzip_mb="$(awk -v b="$gzip_bytes" 'BEGIN { printf "%.1f", b / 1000000 }')"

	echo "::notice title=koala image::${size_mb} MB (gzip ${gzip_mb} MB); /livez ${livez_ms} ms after start; nonroot uid ${uid}"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		cat >>"$GITHUB_STEP_SUMMARY" <<EOF
### koala image \`${version}\`

| Check | Result |
|---|---|
| Size | ${size_mb} MB uncompressed, ${gzip_mb} MB gzip (${size_bytes} bytes) |
| Startup, process start → \`/livez\` 200 | ${livez_ms} ms (limit ${MAX_STARTUP_MS} ms) |
| Startup, \`docker start\` → \`/livez\` 200 | ${livez_docker_ms} ms |
| Process start → \`/readyz\` 200 (database up) | ${readyz_ms} ms |
| User | \`${user}\`, process uid ${uid}; no shell |
| \`koala healthcheck\` | 0 with server, non-zero without; Docker health \`${health}\` |
| \`release-db.sh\` (bootstrap twice, migrate up) | ok |
| SIGTERM → exit | code ${exit_code} after ${stop_ms} ms |
EOF
	fi
}

# --- publish -----------------------------------------------------------------

digest_of() {
	docker buildx imagetools inspect "$1" | awk '/^Digest:/ { print $2; exit }'
}

cmd_publish() {
	local repo="${1:?usage: scripts/image.sh publish <repo> <version> [--latest]}"
	local version="${2:?usage: scripts/image.sh publish <repo> <version> [--latest]}"
	local latest="${3:-}"
	[ -z "$latest" ] || [ "$latest" = --latest ] || fail "unknown argument: $latest"
	check_pins
	local out="${ATTEST_DIR:-dist/attest}"
	local work
	work="$(mktemp -d)"
	mkdir -p "$out"

	local args tags=(-t "$repo:$version")
	mapfile -t args < <(common_args "$version")
	[ -z "$latest" ] || tags+=(-t "$repo:latest")
	docker buildx build "${args[@]}" --provenance=false --sbom=false --push \
		--metadata-file "$work/metadata.json" "${tags[@]}" .

	# One image manifest, not an index (Dokploy/Swarm).
	local media_type digest
	media_type="$(docker buildx imagetools inspect --raw "$repo:$version" | jq -r .mediaType)"
	case "$media_type" in
	application/vnd.oci.image.manifest.v1+json | application/vnd.docker.distribution.manifest.v2+json) ;;
	*) fail "$repo:$version is $media_type; want a single image manifest" ;;
	esac
	digest="$(digest_of "$repo:$version")"
	[ "$digest" = "$(jq -r '."containerimage.digest"' "$work/metadata.json")" ] ||
		fail "registry digest $digest differs from the build's"
	if [ -n "$latest" ]; then
		[ "$(digest_of "$repo:latest")" = "$digest" ] || fail "$repo:latest does not point at $digest"
	fi

	# SBOM and provenance: the same build again (a cache hit), exported as
	# files. BuildKit's local exporter writes each attestation as a JSON file
	# next to the root filesystem; they are told apart by content.
	docker buildx build "${args[@]}" \
		--attest "type=sbom,generator=$SBOM_GENERATOR" \
		--attest type=provenance,mode=max \
		--output "type=local,dest=$work/export" .
	local f sbom="" provenance=""
	while IFS= read -r f; do
		if jq -e '(.predicate // .) | has("spdxVersion")' "$f" >/dev/null 2>&1; then
			sbom="$f"
		elif jq -e '((.predicateType // "") | test("slsa.dev/provenance")) or ((.predicate // .) | has("buildDefinition") or has("buildType"))' "$f" >/dev/null 2>&1; then
			provenance="$f"
		fi
	done < <(find "$work/export" -maxdepth 1 -type f -name '*.json')
	[ -n "$sbom" ] || fail "BuildKit wrote no SPDX SBOM ($(ls "$work/export"))"
	[ -n "$provenance" ] || fail "BuildKit wrote no SLSA provenance ($(ls "$work/export"))"
	jq '.predicate // .' "$sbom" >"$out/sbom.spdx.json"
	cp "$provenance" "$out/provenance.json"

	# The SBOM must see the Go modules inside the binary, not only the
	# distroless packages.
	jq -e '[.packages[].name] | any(. == "github.com/jackc/pgx/v5")' "$out/sbom.spdx.json" >/dev/null ||
		fail "SBOM lists no Go modules from the binary"

	jq -n --arg image "$repo" --arg version "$version" --arg digest "$digest" \
		--arg revision "${GITHUB_SHA:-$(git rev-parse HEAD)}" \
		'{image: $image, version: $version, digest: $digest, reference: ($image + "@" + $digest), revision: $revision}' \
		>"$out/image.json"
	(cd "$out" && sha256sum image.json sbom.spdx.json provenance.json >SHA256SUMS)
	rm -rf "$work"

	echo "published $repo:$version@$digest${latest:+ (and :latest)}"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		local packages
		packages="$(jq '.packages | length' "$out/sbom.spdx.json")"
		cat >>"$GITHUB_STEP_SUMMARY" <<EOF
### Published \`$repo:$version\`

- Digest: \`$digest\` (single linux/amd64 manifest${latest:+; \`:latest\` points at it})
- SBOM: SPDX, ${packages} packages; provenance: SLSA, mode=max
EOF
	fi
}

cmd="${1:-}"
shift || true
case "$cmd" in
build) cmd_build "$@" ;;
smoke) cmd_smoke "$@" ;;
publish) cmd_publish "$@" ;;
*)
	echo "usage: scripts/image.sh build|smoke|publish ..." >&2
	exit 2
	;;
esac

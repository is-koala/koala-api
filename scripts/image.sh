#!/usr/bin/env bash
# Builds, smoke-tests and publishes the koala image (ADR-0023). CI only:
# the verify workflow's image job and the release workflow call it; images
# are never built on the shared Mac (ADR-0004).
#
#   scripts/image.sh build   <version> <ref>             build into the local Docker as <ref>
#   scripts/image.sh smoke   <ref> <version>             run <ref> against Postgres and check it
#   scripts/image.sh publish <repo> <version> <ref>      push, check against <ref>, tag
#
# All builds share their arguments (linux/amd64, VERSION, labels,
# SOURCE_DATE_EPOCH = commit time, rewritten file timestamps), so publish,
# which runs after build and smoke in the same job, is a cache hit of the
# image smoke tested; publish proves it by comparing layers and config.
#
# publish pushes, untagged (push-by-digest), an index holding the
# linux/amd64 image manifest and BuildKit's attestation manifest (SPDX SBOM,
# SLSA provenance, subject = the image manifest). After the checks it
# tags:
#   <repo>:<version>         the image manifest itself, not an index: Docker
#                            Swarm (Dokploy) cannot schedule an index that
#                            carries attestations;
#   <repo>:<version>-attest  the index, so the attestations stay in the
#                            registry next to the image.
# It writes image.json, sbom.spdx.json, provenance.json and SHA256SUMS to
# $ATTEST_DIR (default dist/attest).
set -euo pipefail

# Images BuildKit runs, pinned like the base images (ADR-0023 Karar 6).
SBOM_GENERATOR="docker/buildkit-syft-scanner:1.12.0@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9"
SOURCE_URL="https://github.com/is-koala/koala-api"
# Plan §6.1, Faz 1 exit criterion: startup under 1 s.
MAX_STARTUP_MS="${SMOKE_MAX_STARTUP_MS:-1000}"

fail() {
	echo "::error::image: $*" >&2
	exit 1
}

now_ms() { echo $(($(date +%s%N) / 1000000)); }

revision() { echo "${GITHUB_SHA:-$(git rev-parse HEAD)}"; }

# Arguments every build shares.
common_args() {
	local version="$1"
	printf '%s\n' \
		--platform linux/amd64 \
		--build-arg "VERSION=$version" \
		--build-arg "SOURCE_DATE_EPOCH=$(git log -1 --format=%ct "$(revision)")" \
		--label "org.opencontainers.image.source=$SOURCE_URL" \
		--label "org.opencontainers.image.revision=$(revision)" \
		--label "org.opencontainers.image.version=$version"
}

# Every FROM line pins its image by digest (ADR-0004), whatever its case
# and indentation.
from_images() {
	grep -iE '^[[:space:]]*FROM[[:space:]]' Dockerfile |
		sed -E 's/^[[:space:]]*[Ff][Rr][Oo][Mm][[:space:]]+(--[^[:space:]]+[[:space:]]+)*([^[:space:]]+).*/\2/'
}

check_pins() {
	local image unpinned=""
	while IFS= read -r image; do
		[[ "$image" =~ @sha256:[0-9a-f]{64}$ ]] || unpinned+=" $image"
	done < <(from_images)
	[ -z "$unpinned" ] || fail "Dockerfile base image not pinned by digest:$unpinned"
	[ -n "$(from_images)" ] || fail "no FROM line in Dockerfile"
}

cmd_build() {
	local version="${1:?usage: scripts/image.sh build <version> <ref>}"
	local ref="${2:?usage: scripts/image.sh build <version> <ref>}"
	check_pins
	local args
	mapfile -t args < <(common_args "$version")
	docker buildx build "${args[@]}" --provenance=false --sbom=false \
		--output "type=docker,name=$ref,rewrite-timestamp=true" .
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

http_code() {
	curl -s -o /dev/null -w '%{http_code}' --max-time 1 "$1" 2>/dev/null || true
}

# Polls url until it answers 200 or timeout_ms passes; prints the time of
# the first 200 in ms since the epoch.
wait_200() {
	local url="$1" timeout_ms="$2" start
	start="$(now_ms)"
	until [ "$(http_code "$url")" = 200 ]; do
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

	# 4. koala healthcheck: 0 against the server; without one non-zero, and
	# for the right reason (it probed and was refused, not a config error).
	# Docker's HEALTHCHECK (the same command) turns healthy.
	docker exec "$smoke_container" /koala healthcheck ||
		smoke_logs_and_fail "koala healthcheck failed against a running server"
	local hc_err
	if hc_err="$(docker run --rm --network none -e KOALA_ENV=test "$image" healthcheck 2>&1)"; then
		fail "koala healthcheck succeeded with no server"
	fi
	grep -q 'connection refused' <<<"$hc_err" ||
		fail "koala healthcheck without a server failed for another reason: $hc_err"
	local health="" i
	for i in $(seq 1 40); do
		health="$(docker inspect -f '{{.State.Health.Status}}' "$smoke_container")"
		[ "$health" = healthy ] && break
		sleep 1
	done
	[ "$health" = healthy ] || smoke_logs_and_fail "Docker HEALTHCHECK is '$health' after 40 s, want healthy"

	# 5. SIGTERM (plan §3.10, Faz 1 "/readyz drain davranışı"): /readyz
	# turns 503 while /livez still answers 200 and the listener still
	# accepts, then the process exits 0 within the 50 s stop_grace_period.
	local t_term drain_ms="" code="" exit_code="" stop_ms=""
	docker kill --signal TERM "$smoke_container" >/dev/null
	t_term="$(now_ms)"
	while [ $(($(now_ms) - t_term)) -lt 3000 ]; do
		code="$(http_code http://127.0.0.1:8080/readyz)"
		if [ "$code" = 503 ]; then
			[ "$(http_code http://127.0.0.1:8080/livez)" = 200 ] ||
				smoke_logs_and_fail "/livez is not 200 while /readyz drains"
			drain_ms=$(($(now_ms) - t_term))
			break
		fi
		sleep 0.01
	done
	[ -n "$drain_ms" ] || smoke_logs_and_fail "/readyz did not turn 503 within 3 s of SIGTERM (last: $code)"
	exit_code="$(timeout 50 docker wait "$smoke_container")" ||
		smoke_logs_and_fail "container still running 50 s after SIGTERM"
	stop_ms=$(($(now_ms) - t_term))
	[ "$exit_code" = 0 ] || smoke_logs_and_fail "exit code after SIGTERM is $exit_code, want 0"
	grep -q '"msg":"stopped"' <<<"$(docker logs "$smoke_container" 2>&1)" ||
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
| \`koala healthcheck\` | 0 with server; without one non-zero, connection refused; Docker health \`${health}\` |
| \`release-db.sh\` (bootstrap twice, migrate up) | ok |
| SIGTERM | \`/readyz\` 503 after ${drain_ms} ms with \`/livez\` 200; exit ${exit_code} after ${stop_ms} ms |
EOF
	fi
}

# --- publish -----------------------------------------------------------------

work=""
publish_cleanup() {
	[ -z "$work" ] || rm -rf "$work"
	docker rm -f -v koala-bin-local koala-bin-pushed >/dev/null 2>&1 || true
}

digest_of() {
	docker buildx imagetools inspect "$1" | awk '/^Digest:/ && !d { d = $2 } END { print d }'
}

# The fields of an image config that make the image what it is, in the
# OCI form (.config, .rootfs.diff_ids).
remote_identity() {
	docker buildx imagetools inspect --format '{{json .Image}}' "$1" |
		jq -S '{diff_ids: .rootfs.diff_ids, config: (.config | {User, Entrypoint, Cmd, Env, Labels, ExposedPorts})}'
}

local_identity() {
	docker image inspect "$1" |
		jq -S '.[0] | {diff_ids: .RootFS.Layers, config: (.Config | {User, Entrypoint, Cmd, Env, Labels, ExposedPorts})}'
}

# sha256 of /koala in an image.
binary_digest() {
	local ref="$1" name="$2"
	docker create --name "$name" "$ref" >/dev/null
	docker cp "$name:/koala" - | tar -xOf - koala | sha256sum | awk '{ print "sha256:" $1 }'
	docker rm -f -v "$name" >/dev/null
}

# An attestation view of the single-platform index: buildx prints it bare
# for one platform, keyed by platform otherwise.
attestation() {
	docker buildx imagetools inspect --format "{{json .$2}}" "$1" |
		jq --arg k "$3" 'if has($k) then .[$k] else (.["linux/amd64"] // {})[$k] end'
}

cmd_publish() {
	local repo="${1:?usage: scripts/image.sh publish <repo> <version> <smoke-tested ref>}"
	local version="${2:?usage: scripts/image.sh publish <repo> <version> <smoke-tested ref>}"
	local tested="${3:?usage: scripts/image.sh publish <repo> <version> <smoke-tested ref>}"
	check_pins
	local out="${ATTEST_DIR:-dist/attest}"
	work="$(mktemp -d)"
	trap publish_cleanup EXIT
	mkdir -p "$out"

	# 1. Push, untagged.
	local args
	mapfile -t args < <(common_args "$version")
	docker buildx build "${args[@]}" \
		--attest "type=sbom,generator=$SBOM_GENERATOR" \
		--attest type=provenance,mode=max \
		--output "type=image,name=$repo,push-by-digest=true,name-canonical=true,push=true,rewrite-timestamp=true" \
		--metadata-file "$work/metadata.json" .
	local index image
	index="$(jq -r '."containerimage.digest"' "$work/metadata.json")"
	[[ "$index" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "no digest in the build metadata"

	# 2. Shape: an index with exactly one linux/amd64 image manifest and its
	# attestation manifest(s), bound to that image by digest.
	docker buildx imagetools inspect --raw "$repo@$index" >"$work/index.json"
	jq -e '.mediaType == "application/vnd.oci.image.index.v1+json"' "$work/index.json" >/dev/null ||
		fail "$repo@$index is not an OCI index"
	image="$(jq -r '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")]
		| if length == 1 then .[0].digest else "" end' "$work/index.json")"
	[ -n "$image" ] || fail "index does not hold exactly one linux/amd64 image"
	jq -e --arg d "$image" '
		[.manifests[] | select(.digest != $d)] as $rest
		| ($rest | length) >= 1
		and ($rest | all(.annotations["vnd.docker.reference.type"] == "attestation-manifest"
			and .annotations["vnd.docker.reference.digest"] == $d))' "$work/index.json" >/dev/null ||
		fail "index holds something other than the image and its attestations"
	case "$(docker buildx imagetools inspect --raw "$repo@$image" | jq -r .mediaType)" in
	application/vnd.oci.image.manifest.v1+json | application/vnd.docker.distribution.manifest.v2+json) ;;
	*) fail "$repo@$image is not a single image manifest" ;;
	esac

	# 3. The pushed image is the one smoke tested: same layers, same config,
	# same binary.
	if ! diff <(local_identity "$tested") <(remote_identity "$repo@$image") >"$work/identity.diff"; then
		cat "$work/identity.diff" >&2
		fail "pushed image differs from the smoke-tested $tested"
	fi
	docker pull -q "$repo@$image" >/dev/null
	local bin_tested bin_pushed
	bin_tested="$(binary_digest "$tested" koala-bin-local)"
	bin_pushed="$(binary_digest "$repo@$image" koala-bin-pushed)"
	[ "$bin_tested" = "$bin_pushed" ] || fail "/koala differs: tested $bin_tested, pushed $bin_pushed"

	# 4. SBOM and provenance from the registry. The SBOM sees the Go modules
	# inside the binary; the provenance names the pinned base images.
	attestation "$repo@$index" SBOM SPDX >"$out/sbom.spdx.json"
	attestation "$repo@$index" Provenance SLSA >"$out/provenance.json"
	jq -e '[.packages[]?.name] | any(. == "github.com/jackc/pgx/v5")' "$out/sbom.spdx.json" >/dev/null ||
		fail "SBOM lists no Go modules from the binary"
	jq -c '.materials // .buildDefinition.resolvedDependencies // empty' "$out/provenance.json"
	local base
	while IFS= read -r base; do
		# No pipe into grep -q: its early exit fails the pipeline (pipefail).
		jq -e --arg h "${base##*sha256:}" '[.. | strings] | any(contains($h))' "$out/provenance.json" >/dev/null ||
			fail "provenance does not name base image $base"
	done < <(from_images)

	# 5. Tags, only now: the image manifest itself (no index) and the index.
	docker buildx imagetools create --prefer-index=false -t "$repo:$version" "$repo@$image"
	docker buildx imagetools create -t "$repo:$version-attest" "$repo@$index"
	[ "$(digest_of "$repo:$version")" = "$image" ] || fail "$repo:$version does not point at $image"
	[ "$(digest_of "$repo:$version-attest")" = "$index" ] || fail "$repo:$version-attest does not point at $index"

	jq -n --arg image "$repo" --arg version "$version" --arg digest "$image" --arg index "$index" \
		--arg binary "$bin_pushed" --arg revision "$(revision)" \
		'{image: $image, version: $version, digest: $digest,
		  reference: ($image + ":" + $version + "@" + $digest),
		  attestations: ($image + ":" + $version + "-attest@" + $index),
		  binary_sha256: $binary, revision: $revision}' >"$out/image.json"
	(cd "$out" && sha256sum image.json sbom.spdx.json provenance.json >SHA256SUMS)

	echo "published $repo:$version@$image (attestations $repo:$version-attest@$index)"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		local packages
		packages="$(jq '.packages | length' "$out/sbom.spdx.json")"
		cat >>"$GITHUB_STEP_SUMMARY" <<EOF
### Published \`$repo:$version\`

- Image: \`$repo:$version@$image\` (single linux/amd64 manifest; same layers, config and \`/koala\` as the smoke-tested image)
- Attestations: \`$repo:$version-attest@$index\` (SPDX SBOM, ${packages} packages; SLSA provenance, mode=max; subject $image)
- \`/koala\`: \`$bin_pushed\`
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

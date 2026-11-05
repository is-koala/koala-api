#!/usr/bin/env bash
# Version rules of a release (ADR-0023), used by the release workflow's
# gate and proven by the verify workflow's image job:
#
#   scripts/release.sh check-version <version> <file with existing tags>
#       <version> is vMAJOR.MINOR.PATCH[-PRERELEASE] (SemVer 2.0.0, no
#       build metadata), is not an existing tag, and is greater than every
#       existing SemVer tag. Prints "prerelease=true|false".
#   scripts/release.sh self-test
#       the precedence cases of SemVer §11 and the checks above.
set -euo pipefail
# SemVer compares alphanumeric identifiers in ASCII order.
export LC_ALL=C

semver_re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$'

fail() {
	echo "::error::release: $*" >&2
	exit 1
}

is_semver() { [[ "$1" =~ $semver_re ]]; }

# cmp_ident a b: SemVer §11.4 for one pre-release identifier.
cmp_ident() {
	local a="$1" b="$2" an=0 bn=0
	[[ "$a" =~ ^[0-9]+$ ]] && an=1
	[[ "$b" =~ ^[0-9]+$ ]] && bn=1
	if ((an && bn)); then
		if ((10#$a < 10#$b)); then echo -1; elif ((10#$a > 10#$b)); then echo 1; else echo 0; fi
	elif ((an)); then
		echo -1 # numeric identifiers sort before alphanumeric ones
	elif ((bn)); then
		echo 1
	elif [[ "$a" < "$b" ]]; then
		echo -1
	elif [[ "$a" > "$b" ]]; then
		echo 1
	else
		echo 0
	fi
}

# semver_cmp a b: prints -1, 0 or 1 (SemVer §11 precedence).
semver_cmp() {
	local a="$1" b="$2" i
	[[ "$a" =~ $semver_re ]] || fail "not SemVer: $a"
	local -a av=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
	local ap="${BASH_REMATCH[5]}"
	[[ "$b" =~ $semver_re ]] || fail "not SemVer: $b"
	local -a bv=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}")
	local bp="${BASH_REMATCH[5]}"
	for i in 0 1 2; do
		if ((av[i] < bv[i])); then echo -1 && return; fi
		if ((av[i] > bv[i])); then echo 1 && return; fi
	done
	# A version without pre-release is greater than one with.
	if [ -z "$ap" ] && [ -z "$bp" ]; then echo 0 && return; fi
	if [ -z "$ap" ]; then echo 1 && return; fi
	if [ -z "$bp" ]; then echo -1 && return; fi
	local -a ai bi
	IFS=. read -r -a ai <<<"$ap"
	IFS=. read -r -a bi <<<"$bp"
	local n=${#ai[@]} c
	((${#bi[@]} < n)) && n=${#bi[@]}
	for ((i = 0; i < n; i++)); do
		c="$(cmp_ident "${ai[i]}" "${bi[i]}")"
		if [ "$c" != 0 ]; then echo "$c" && return; fi
	done
	if ((${#ai[@]} < ${#bi[@]})); then echo -1; elif ((${#ai[@]} > ${#bi[@]})); then echo 1; else echo 0; fi
}

check_version() {
	local version="$1" tags_file="$2" tag max=""
	is_semver "$version" || fail "$version is not vMAJOR.MINOR.PATCH[-PRERELEASE]"
	while IFS= read -r tag; do
		[ -n "$tag" ] || continue
		[ "$tag" != "$version" ] || fail "tag $version already exists"
		is_semver "$tag" || continue
		if [ -z "$max" ] || [ "$(semver_cmp "$tag" "$max")" = 1 ]; then max="$tag"; fi
	done <"$tags_file"
	if [ -n "$max" ] && [ "$(semver_cmp "$version" "$max")" != 1 ]; then
		fail "$version is not greater than the latest release $max"
	fi
	if [[ "$version" == *-* ]]; then echo prerelease=true; else echo prerelease=false; fi
}

self_test() {
	local -a ordered=(v0.9.0 v1.0.0-alpha v1.0.0-alpha.1 v1.0.0-alpha.beta v1.0.0-beta v1.0.0-beta.2
		v1.0.0-beta.11 v1.0.0-rc.1 v1.0.0 v1.0.1 v1.1.0 v1.10.0 v2.0.0)
	local i j want got
	for ((i = 0; i < ${#ordered[@]}; i++)); do
		for ((j = 0; j < ${#ordered[@]}; j++)); do
			want=0
			((i < j)) && want=-1
			((i > j)) && want=1
			got="$(semver_cmp "${ordered[i]}" "${ordered[j]}")"
			[ "$got" = "$want" ] || fail "cmp ${ordered[i]} ${ordered[j]} = $got, want $want"
		done
	done
	local bad
	for bad in 1.0.0 v1.0 v01.0.0 v1.0.0+build v1.0.0- v1.0.0-rc..1 latest; do
		if is_semver "$bad"; then fail "$bad accepted as SemVer"; fi
	done

	local tags
	tags="$(mktemp)"
	printf '%s\n' v0.1.0 v0.2.0-rc.1 v0.10.0 not-a-version >"$tags"
	[ "$(check_version v0.10.1 "$tags")" = prerelease=false ] || fail "v0.10.1 after v0.10.0 refused"
	[ "$(check_version v0.11.0-rc.1 "$tags")" = prerelease=true ] || fail "v0.11.0-rc.1 refused"
	local refused
	for refused in v0.10.0 v0.9.0 v0.10.0-rc.1 0.11.0; do
		if (check_version "$refused" "$tags" >/dev/null 2>&1); then fail "$refused accepted"; fi
	done
	rm -f "$tags"
	echo "release.sh self-test: ok"
}

cmd="${1:-}"
shift || true
case "$cmd" in
check-version) check_version "${1:?version}" "${2:?tags file}" ;;
self-test) self_test ;;
*)
	echo "usage: scripts/release.sh check-version <version> <tags-file> | self-test" >&2
	exit 2
	;;
esac

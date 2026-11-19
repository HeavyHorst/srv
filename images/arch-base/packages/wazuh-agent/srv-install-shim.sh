#!/usr/bin/env bash
# Stands in for install, chown and chgrp while upstream install.sh runs in
# package(). It confines every write to $SRV_PKGDIR, applies modes and content,
# and appends the requested owners to $SRV_OWNERSHIP_LOG instead of applying
# them, because the wazuh account exists only on the target system.
#
# Log lines:  <d|f|z> <mode> <user> <group> <path>   (from install)
#             own <user|-> <group|-> <path>           (from chown/chgrp)
# Paths are logged relative to $SRV_PKGDIR, i.e. as installed on the target.
set -euo pipefail

: "${SRV_PKGDIR:?}" "${SRV_OWNERSHIP_LOG:?}"
tool="${0##*/}"

target_path() {
	local path
	path="$(realpath -m -- "$1")"
	case "${path}" in
	"${SRV_PKGDIR}"/*) printf '%s' "${path#"${SRV_PKGDIR}"}" ;;
	*)
		echo "srv-install-shim: refusing ${tool} outside the package root: $1" >&2
		exit 1
		;;
	esac
}

valid_name() {
	[[ "$1" =~ ^([a-z_][a-z0-9_-]*|0)$ ]] || {
		echo "srv-install-shim: unsupported owner '$1'" >&2
		exit 1
	}
	[[ "$1" == 0 ]] && printf root || printf '%s' "$1"
}

record_owner() {
	local spec="$1" user=- group=- path target
	shift
	case "${tool}" in
	chgrp) group="$(valid_name "${spec}")" || exit 1 ;;
	chown)
		user="${spec%%:*}"
		[[ "${spec}" == *:* ]] && group="${spec#*:}"
		user="$(valid_name "${user}")" || exit 1
		if [[ "${group}" == - || -z "${group}" ]]; then
			group=-
		else
			group="$(valid_name "${group}")" || exit 1
		fi
		;;
	esac
	for path in "$@"; do
		[[ -e "${path}" || -L "${path}" ]] || {
			echo "srv-install-shim: ${tool} on missing path ${path}" >&2
			exit 1
		}
		target="$(target_path "${path}")" || exit 1
		printf 'own %s %s %s\n' "${user}" "${group}" "${target}" >>"${SRV_OWNERSHIP_LOG}"
	done
}

case "${tool}" in
chown | chgrp)
	[[ "${1:-}" != -* ]] || {
		echo "srv-install-shim: unsupported ${tool} option $1" >&2
		exit 1
	}
	[[ $# -ge 2 ]] || exit 1
	record_owner "$@"
	exit 0
	;;
install) ;;
*)
	echo "srv-install-shim: unknown tool ${tool}" >&2
	exit 1
	;;
esac

directory=0 mode="" user=root group=root
args=()
while [[ $# -gt 0 ]]; do
	case "$1" in
	-d) directory=1 ;;
	-b | -p) args+=("$1") ;;
	-m) mode="$2"; shift ;;
	-o) user="$(valid_name "$2")" || exit 1; shift ;;
	-g) group="$(valid_name "$2")" || exit 1; shift ;;
	--) shift; break ;;
	-*)
		echo "srv-install-shim: unsupported install option $1" >&2
		exit 1
		;;
	*) break ;;
	esac
	shift
done
[[ $# -ge 1 ]] || exit 1
if [[ -n "${mode}" ]]; then
	[[ "${mode}" =~ ^[0-7]{3,4}$ ]] || {
		echo "srv-install-shim: unsupported mode ${mode}" >&2
		exit 1
	}
	args+=(-m "${mode}")
	mode="$(printf '%04o' "$((8#${mode}))")"
fi

if [[ "${directory}" == 1 ]]; then
	for path in "$@"; do
		target="$(target_path "${path}")" || exit 1
		printf 'd %s %s %s %s\n' "${mode:-0755}" "${user}" "${group}" "${target}" >>"${SRV_OWNERSHIP_LOG}"
	done
	exec /usr/bin/install -d "${args[@]}" -- "$@"
fi

[[ $# -ge 2 ]] || exit 1
dest="${!#}"
sources=("${@:1:$#-1}")
for source in "${sources[@]}"; do
	if [[ ${#sources[@]} -gt 1 || -d "${dest}" ]]; then
		path="${dest%/}/${source##*/}"
	else
		path="${dest}"
	fi
	type=z
	[[ "${source}" == /dev/null ]] && type=f
	target="$(target_path "${path}")" || exit 1
	printf '%s %s %s %s %s\n' "${type}" "${mode:-0755}" "${user}" "${group}" "${target}" >>"${SRV_OWNERSHIP_LOG}"
done
exec /usr/bin/install "${args[@]}" -- "$@"

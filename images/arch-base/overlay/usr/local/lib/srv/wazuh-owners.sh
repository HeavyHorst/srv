#!/usr/bin/env bash
# Applies the owners and modes the wazuh-agent package records in
# /usr/lib/tmpfiles.d/wazuh-agent.conf, recreating its directories and empty
# log files, at image build time and before the agent starts on every boot.
#
# systemd-tmpfiles refuses that file: upstream puts root-owned files in
# wazuh-owned directories, an "unsafe path transition" it guards against
# because the wazuh user could swap a path for a symlink. The image masks the
# file with /etc/tmpfiles.d/wazuh-agent.conf -> /dev/null, and this script
# applies it instead. It runs before any wazuh process (srv-wazuh-owners.service
# is ordered before the agent), refuses every path with a symlink in it, and
# never follows one.
#
# Only d (directory), f (empty file) and z (adjust existing) lines under
# /var/ossec with octal modes are accepted; the sticky bit is allowed (the
# package's /var/ossec/tmp is 1770), setuid and setgid are not. Anything else fails.
set -euo pipefail

ROOT="${SRV_WAZUH_ENROLL_ROOT:-}"
CONF="${ROOT}/usr/lib/tmpfiles.d/wazuh-agent.conf"
NAME_RE='^[a-z_][a-z0-9_-]{0,31}$'

fail() {
	echo "srv-wazuh-owners: $*" >&2
	exit 1
}

# Every component from /var/ossec down must be a real file or directory.
check_path() {
	local path="$1" part="${ROOT}"
	local -a parts
	IFS=/ read -r -a parts <<<"${path#/}"
	for component in "${parts[@]}"; do
		part="${part}/${component}"
		if [[ -L "${part}" ]]; then
			fail "refusing ${path}: ${part#"${ROOT}"} is a symlink"
		fi
	done
}

[[ -f "${CONF}" && ! -L "${CONF}" ]] || fail "missing ${CONF#"${ROOT}"}"
while read -r type path mode user group rest; do
	[[ -z "${type}" || "${type}" == \#* ]] && continue
	[[ "${type}" =~ ^[dfz]$ ]] || fail "unsupported entry type ${type} for ${path}"
	[[ "${path}" =~ ^/var/ossec(/[A-Za-z0-9._+@-]+)*$ && "${path}" != */../* && "${path}" != */.. ]] || fail "refusing path ${path}"
	[[ "${mode}" =~ ^[01][0-7]{3}$ ]] || fail "invalid mode ${mode} for ${path}"
	[[ "${user}" =~ ${NAME_RE} && "${group}" =~ ${NAME_RE} ]] || fail "invalid owner ${user}:${group} for ${path}"
	[[ "${rest}" == "-" ]] || fail "unsupported argument for ${path}"
	check_path "${path}"
	target="${ROOT}${path}"
	case "${type}" in
	d)
		if [[ ! -e "${target}" ]]; then
			mkdir -m 0700 -- "${target}"
		fi
		[[ -d "${target}" ]] || fail "${path} is not a directory"
		;;
	f)
		if [[ ! -e "${target}" ]]; then
			(umask 077 && : >"${target}")
		fi
		[[ -f "${target}" ]] || fail "${path} is not a regular file"
		;;
	z)
		# Optional files the package may not ship on every build.
		[[ -e "${target}" ]] || continue
		;;
	esac
	# The path was checked above; re-check right before changing it.
	[[ -L "${target}" ]] && fail "refusing ${path}: became a symlink"
	chown -h -- "${user}:${group}" "${target}"
	chmod -- "${mode}" "${target}"
done <"${CONF}"

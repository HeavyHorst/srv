#!/usr/bin/env bash
# Installs a controller-delivered Wazuh agent identity and starts the agent.
#
# The guest never enrolls itself. The srv controller registers the agent with
# the manager API (POST /agents, which goes through the manager's local authd),
# reads its key (GET /agents/{id}/key), and writes this file over the guest's
# authenticated control channel, as root with mode 0600:
#
#   /run/srv-wazuh/enrollment.json
#   {
#     "version": 1,
#     "agent_id": "012",                  3-8 digits, not 000
#     "agent_name": "<guest hostname>",   must equal this guest's hostname
#     "agent_key": "<base64>",            the key string from GET /agents/{id}/key,
#                                         decoding to "<id> <name> <ip> <64 hex>"
#     "manager_address": "<host or IPv4>",
#     "manager_port": 1514
#   }
#
# It then starts srv-wazuh-enroll.service. The file is deleted whether or not
# it is accepted.
#
# Usage: wazuh-enroll.sh enroll [FILE]   install the identity and start the agent
#        wazuh-enroll.sh check           exit 0 only for an approved identity
set -euo pipefail

ROOT="${SRV_WAZUH_ENROLL_ROOT:-}"
INPUT_DEFAULT="${ROOT}/run/srv-wazuh/enrollment.json"
ETC_DIR="${ROOT}/var/ossec/etc"
KEYS_FILE="${ETC_DIR}/client.keys"
CONFIG_FILE="${ETC_DIR}/ossec.conf"
APPROVAL_FILE="${ROOT}/var/lib/srv/wazuh-enrolled"
HOSTNAME_FILE="${ROOT}/proc/sys/kernel/hostname"
PLACEHOLDER="SRV_WAZUH_MANAGER_ADDRESS"
KEY_LINE_RE='^(0*[1-9][0-9]{0,7}) ([A-Za-z0-9._-]{1,128}) (any|[0-9]{1,3}(\.[0-9]{1,3}){3}(/[0-9]{1,2})?) ([0-9A-Fa-f]{64})$'

fail() {
	echo "srv-wazuh-enroll: $*" >&2
	exit 1
}

# Writes stdin to PATH via a sibling temporary file and rename, so readers see
# either the old or the new file.
atomic_write() {
	local path="$1" mode="$2" group="$3" tmp
	tmp="$(mktemp "${path%/*}/.${path##*/}.XXXXXX")"
	if ! cat >"${tmp}" || ! chown "root:${group}" "${tmp}" || ! chmod "${mode}" "${tmp}" || ! sync "${tmp}"; then
		rm -f "${tmp}"
		fail "cannot write ${path}"
	fi
	mv -f "${tmp}" "${path}"
	sync "${path%/*}"
}

approved_identity() {
	local line expected
	[[ -f "${KEYS_FILE}" && ! -L "${KEYS_FILE}" ]] || fail "no agent identity at ${KEYS_FILE}"
	[[ "$(wc -l <"${KEYS_FILE}")" == 1 ]] || fail "client.keys must hold exactly one agent"
	IFS= read -r line <"${KEYS_FILE}"
	[[ "${line}" =~ ${KEY_LINE_RE} ]] || fail "client.keys is malformed"
	[[ -f "${APPROVAL_FILE}" && ! -L "${APPROVAL_FILE}" ]] || fail "agent identity was not installed by srv-wazuh-enroll"
	read -r expected _ <"${APPROVAL_FILE}"
	[[ "$(sha256sum <"${KEYS_FILE}")" == "${expected}  -" ]] || fail "client.keys does not match the approved identity"
	! grep -qF "${PLACEHOLDER}" "${CONFIG_FILE}" || fail "no manager address is configured"
}

enroll() {
	local input="$1" json id name key decoded address port line
	exec 9>"${ROOT}/run/srv-wazuh/enroll.lock"
	flock -w 10 9 || fail "another enrollment is running"
	# The identity holds the agent key: never leave it behind.
	# shellcheck disable=SC2064 # expand now: input is local to this function
	trap "rm -f -- $(printf '%q' "${input}")" EXIT

	[[ -f "${input}" && ! -L "${input}" ]] || fail "no enrollment file at ${input}"
	[[ "$(stat -c '%u %a' -- "${input}")" =~ ^$(id -u)\ [46]00$ ]] ||
		fail "enrollment file must be owned by $(id -un) with mode 0600"
	(($(stat -c %s -- "${input}") <= 4096)) || fail "enrollment file is too large"

	json="$(jq -ce '
		select(type == "object" and .version == 1
			and (.agent_id | type) == "string" and (.agent_name | type) == "string"
			and (.agent_key | type) == "string" and (.manager_address | type) == "string"
			and (.manager_port | type) == "number"
			and ([.agent_id, .agent_name, .agent_key, .manager_address] | all(test("^[!-~]+$"))))
		| [.agent_id, .agent_name, .agent_key, .manager_address, (.manager_port | tostring)]
	' "${input}")" || fail "enrollment file is not a version 1 identity"
	{
		read -r id
		read -r name
		read -r key
		read -r address
		read -r port
	} < <(jq -r '.[]' <<<"${json}")

	[[ "${id}" =~ ^[0-9]{3,8}$ && "${id}" =~ [1-9] ]] || fail "invalid agent_id"
	[[ "${name}" =~ ^[A-Za-z0-9._-]{1,128}$ ]] || fail "invalid agent_name"
	[[ "${name}" == "$(<"${HOSTNAME_FILE}")" ]] || fail "agent_name ${name} is not this guest"
	[[ "${address}" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*$ && ${#address} -le 253 ]] ||
		fail "invalid manager_address"
	[[ "${port}" =~ ^[1-9][0-9]{0,4}$ ]] && ((port <= 65535)) || fail "invalid manager_port"
	[[ "${key}" =~ ^[A-Za-z0-9+/]{1,512}={0,2}$ ]] || fail "agent_key is not base64"
	decoded="$(base64 -d <<<"${key}" 2>/dev/null)" || fail "agent_key is not base64"
	[[ "${decoded}" =~ ${KEY_LINE_RE} ]] || fail "agent_key does not decode to a client.keys entry"
	[[ "${BASH_REMATCH[1]}" == "${id}" && "${BASH_REMATCH[2]}" == "${name}" ]] ||
		fail "agent_key belongs to agent ${BASH_REMATCH[1]} ${BASH_REMATCH[2]}"
	line="${decoded}"

	printf '%s\n' "${line}" | atomic_write "${KEYS_FILE}" 0640 wazuh
	sed -E \
		-e "s|^( *<address>).*(</address><!-- srv-managed -->)$|\\1${address}\\2|" \
		-e "s|^( *<port>).*(</port><!-- srv-managed -->)$|\\1${port}\\2|" \
		"${CONFIG_FILE}" | atomic_write "${CONFIG_FILE}" 0660 wazuh
	grep -qF "<address>${address}</address><!-- srv-managed -->" "${CONFIG_FILE}" ||
		fail "ossec.conf has no srv-managed manager address"
	sha256sum <"${KEYS_FILE}" | atomic_write "${APPROVAL_FILE}" 0600 root

	systemctl enable wazuh-agent.service
	systemctl restart wazuh-agent.service
	echo "srv-wazuh-enroll: agent ${id} ${name} reports to ${address}:${port}" >&2
}

case "${1:-}" in
enroll) enroll "${2:-${INPUT_DEFAULT}}" ;;
check) approved_identity ;;
*) fail "usage: $0 enroll [FILE] | check" ;;
esac

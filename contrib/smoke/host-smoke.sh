#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat <<'EOF'
usage: sudo ./contrib/smoke/host-smoke.sh

Runs a host-level smoke test against the systemd-managed srv deployment.

Environment overrides:
  ENV_PATH                      default: /etc/srv/srv.env
  SMOKE_SSH_HOST                default: SRV_HOSTNAME from ENV_PATH, else srv
  SSH_USER                      default: root
  INSTANCE_NAME                 default: smoke-<utc timestamp>-<pid>
  POOL_NAME                     default: <INSTANCE_NAME>-pool
  POOLED_INSTANCE_NAME          default: <INSTANCE_NAME>-pooled
  POOLED_PEER_INSTANCE_NAME     default: <INSTANCE_NAME>-pooled-peer
  POOL_SIZE_MIB                 default: 2048
  POOLED_VM_MEMORY_MIB          default: 1024
  POOLED_VM_RESIZE_MIB          default: 1536
  POOLED_VM_OVERSIZE_MIB        default: <POOL_SIZE_MIB> + 512
  ARTIFACT_ROOT                 default: /var/tmp/srv-smoke
  ARTIFACT_DIR                  default: <ARTIFACT_ROOT>/<INSTANCE_NAME>
  KEEP_FAILED                   default: 0
  POLL_INTERVAL_SECONDS         default: 5
  READY_TIMEOUT_BUFFER_SECONDS  default: 30
  READY_TIMEOUT_SECONDS         default: derived from SRV_GUEST_READY_TIMEOUT
  GUEST_SSH_READY_TIMEOUT       default: 30
  SSH_CONNECT_TIMEOUT           default: 10
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
	usage
	exit 0
fi

if [[ "$#" -ne 0 ]]; then
	usage >&2
	exit 1
fi

ENV_PATH="${ENV_PATH:-/etc/srv/srv.env}"
ARTIFACT_ROOT="${ARTIFACT_ROOT:-/var/tmp/srv-smoke}"
KEEP_FAILED="${KEEP_FAILED:-0}"
POLL_INTERVAL_SECONDS="${POLL_INTERVAL_SECONDS:-5}"
READY_TIMEOUT_BUFFER_SECONDS="${READY_TIMEOUT_BUFFER_SECONDS:-30}"
READY_TIMEOUT_SECONDS="${READY_TIMEOUT_SECONDS:-}"
GUEST_SSH_READY_TIMEOUT="${GUEST_SSH_READY_TIMEOUT:-30}"
SSH_USER="${SSH_USER:-root}"
SSH_CONNECT_TIMEOUT="${SSH_CONNECT_TIMEOUT:-10}"

RUN_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RUN_STARTED_EPOCH="$(date -u +%s)"

INSTANCE_ATTEMPTED=0
CLEANUP_COMPLETE=0
POOL_ATTEMPTED=0
POOL_CLEANUP_COMPLETE=0
CLEANUP_INSTANCE_NAMES=()
TAILSCALE_NAME=""
TAILSCALE_IP=""
INSTANCE_TAP_DEVICE=""
BACKUP_ID=""
BACKUP_PATH=""
CURRENT_MEMORY_MODE="fixed"
CURRENT_POOL_NAME=""
CURRENT_MEMORY_MIB=0
BACKUP_MARKER_PATH="/root/srv-smoke-backup-marker"
POST_BACKUP_PATH="/root/srv-smoke-post-backup-only"

log() {
	printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

fail() {
	log "FAIL: $*"
	exit 1
}

require_root() {
	if [[ "$(id -u)" -ne 0 ]]; then
		echo "run this smoke test as root" >&2
		exit 1
	fi
}

require_commands() {
	local missing=()
	local cmd
	for cmd in awk cp curl date grep journalctl ls mkdir sed ssh systemctl tailscale; do
		if ! command -v "${cmd}" >/dev/null 2>&1; then
			missing+=("${cmd}")
		fi
	done
	if [[ "${#missing[@]}" -gt 0 ]]; then
		echo "missing required commands: ${missing[*]}" >&2
		exit 1
	fi
}

load_env() {
	if [[ ! -f "${ENV_PATH}" ]]; then
		return
	fi
	set -a
	# shellcheck disable=SC1090
	. "${ENV_PATH}"
	set +a
}

duration_to_seconds() {
	local remaining="$1"
	local total=0
	local value unit

	if [[ -z "${remaining}" ]]; then
		printf '120\n'
		return 0
	fi

	while [[ -n "${remaining}" ]]; do
		if [[ "${remaining}" =~ ^([0-9]+)(h|m|s)(.*)$ ]]; then
			value="${BASH_REMATCH[1]}"
			unit="${BASH_REMATCH[2]}"
			remaining="${BASH_REMATCH[3]}"
			case "${unit}" in
				h)
					total=$(( total + value * 3600 ))
					;;
				m)
					total=$(( total + value * 60 ))
					;;
				s)
					total=$(( total + value ))
					;;
			esac
			continue
		fi
		return 1
	done

	printf '%s\n' "${total}"
}

derive_ready_timeout_seconds() {
	if [[ -n "${READY_TIMEOUT_SECONDS}" ]]; then
		printf '%s\n' "${READY_TIMEOUT_SECONDS}"
		return 0
	fi
	if duration_to_seconds "${SRV_GUEST_READY_TIMEOUT:-2m}"; then
		return 0
	fi
	printf '120\n'
}

run_capture() {
	local label="$1"
	shift
	local stdout_path="${ARTIFACT_DIR}/${label}.stdout"
	local stderr_path="${ARTIFACT_DIR}/${label}.stderr"
	local rc_path="${ARTIFACT_DIR}/${label}.rc"
	local rc

	set +e
	"$@" >"${stdout_path}" 2>"${stderr_path}"
	rc=$?
	set -e
	printf '%s\n' "${rc}" >"${rc_path}"
	return "${rc}"
}

srv_ssh_capture() {
	local label="$1"
	shift
	run_capture "${label}" ssh "${ssh_opts[@]}" "${SSH_USER}@${CONTROL_HOST}" "$@"
}

guest_ssh_capture() {
	local label="$1"
	local remote_command="$2"
	local guest_host="${TAILSCALE_NAME:-${TAILSCALE_IP}}"
	run_capture "${label}" ssh "${ssh_opts[@]}" "${SSH_USER}@${guest_host}" "${remote_command}"
}

capture_best_effort() {
	local label="$1"
	shift
	run_capture "${label}" "$@" || true
}

capture_best_effort_ssh() {
	local label="$1"
	shift
	srv_ssh_capture "${label}" "$@" || true
}

extract_field() {
	local file_path="$1"
	local key="$2"
	awk -v prefix="${key}: " 'index($0, prefix) == 1 { print substr($0, length(prefix) + 1); exit }' "${file_path}"
}

copy_capture_result() {
	local source_label="$1"
	local dest_label="$2"
	cp "${ARTIFACT_DIR}/${source_label}.stdout" "${ARTIFACT_DIR}/${dest_label}.stdout"
	cp "${ARTIFACT_DIR}/${source_label}.stderr" "${ARTIFACT_DIR}/${dest_label}.stderr"
	cp "${ARTIFACT_DIR}/${source_label}.rc" "${ARTIFACT_DIR}/${dest_label}.rc"
}

register_cleanup_instance() {
	local name="$1"
	local existing
	for existing in "${CLEANUP_INSTANCE_NAMES[@]}"; do
		if [[ "${existing}" == "${name}" ]]; then
			return 0
		fi
	done
	CLEANUP_INSTANCE_NAMES+=("${name}")
}

wait_for_ready() {
	local label="$1"
	local latest_label="${label}-latest"
	local deadline=$(( $(date -u +%s) + READY_TIMEOUT_SECONDS ))
	local state

	while :; do
		if srv_ssh_capture "${latest_label}" inspect "${INSTANCE_NAME}"; then
			state="$(extract_field "${ARTIFACT_DIR}/${latest_label}.stdout" state || true)"
			TAILSCALE_NAME="$(extract_field "${ARTIFACT_DIR}/${latest_label}.stdout" tailscale-name || true)"
			TAILSCALE_IP="$(extract_field "${ARTIFACT_DIR}/${latest_label}.stdout" tailscale-ip || true)"
			INSTANCE_TAP_DEVICE="$(extract_field "${ARTIFACT_DIR}/${latest_label}.stdout" tap-device || true)"
			if [[ "${state}" == "ready" && -n "${TAILSCALE_NAME}" && -n "${TAILSCALE_IP}" ]]; then
				copy_capture_result "${latest_label}" "${label}"
				copy_capture_result "${latest_label}" "inspect-final"
				return 0
			fi
		else
			if grep -q 'does not exist' "${ARTIFACT_DIR}/${latest_label}.stderr"; then
				fail "instance ${INSTANCE_NAME} was not visible while waiting for ${label}"
			fi
		fi

		if [[ "$(date -u +%s)" -ge "${deadline}" ]]; then
			fail "instance ${INSTANCE_NAME} did not reach ready state during ${label} before timeout"
		fi

		sleep "${POLL_INTERVAL_SECONDS}"
	done
}

trim_file() {
	tr -d '\n' <"$1"
}

json_number_for_key() {
	local file_path="$1"
	local key="$2"
	tr -d '[:space:]' <"${file_path}" | sed -n "s/.*\"${key}\":\([0-9][0-9]*\).*/\1/p"
}

strip_ansi() {
	sed -r $'s/\x1B\[[0-9;]*m//g' "$1"
}

box_value_for_label() {
	local file_path="$1"
	local label="$2"
	strip_ansi "${file_path}" | awk -F '│' -v label="${label}" '
		NF < 3 { next }
		{
			gsub(/^[[:space:]]+|[[:space:]]+$/, "", $2)
			if ($2 != label) {
				next
			}
			gsub(/^[[:space:]]+|[[:space:]]+$/, "", $3)
			print $3
			exit
		}
	'
}

set_active_instance() {
	INSTANCE_NAME="$1"
	INSTANCE_DIR="${SRV_DATA_DIR}/instances/${INSTANCE_NAME}"
	JAILER_WORKSPACE_DIR="${JAILER_BASE_DIR}/${JAILER_EXEC_NAME}/${INSTANCE_NAME}"
	TAILSCALE_NAME=""
	TAILSCALE_IP=""
	INSTANCE_TAP_DEVICE=""
}

list_output_contains() {
	local file_path="$1"
	local name="$2"
	local state="${3:-}"
	awk -v name="${name}" -v state="${state}" '
		index($0, name) == 0 { next }
		state != "" && index($0, state) == 0 { next }
		{ found = 1; exit }
		END { exit(found ? 0 : 1) }
	' "${file_path}"
}

assert_instance_listed() {
	local label="$1"
	local expected_state="$2"
	if ! srv_ssh_capture "${label}" list; then
		fail "list failed during ${label}"
	fi
	if ! list_output_contains "${ARTIFACT_DIR}/${label}.stdout" "${INSTANCE_NAME}" "${expected_state}"; then
		fail "instance ${INSTANCE_NAME} did not appear in list output with state ${expected_state} during ${label}"
	fi
}

verify_guest_ssh() {
	local label="$1"
	local deadline=$(( $(date -u +%s) + GUEST_SSH_READY_TIMEOUT ))

	while :; do
		if guest_ssh_capture "${label}" "id -u && uname -a"; then
			if grep -q '^0$' "${ARTIFACT_DIR}/${label}.stdout"; then
				return 0
			fi
			fail "guest SSH check during ${label} did not report root uid"
		fi

		if [[ "$(date -u +%s)" -ge "${deadline}" ]]; then
			fail "guest SSH check failed during ${label}"
		fi

		sleep "${POLL_INTERVAL_SECONDS}"
	done
}

verify_guest_file_content() {
	local label="$1"
	local path="$2"
	local expected="$3"

	if ! guest_ssh_capture "${label}" "cat '${path}'"; then
		fail "guest file ${path} could not be read during ${label}"
	fi
	if [[ "$(trim_file "${ARTIFACT_DIR}/${label}.stdout")" != "${expected}" ]]; then
		fail "guest file ${path} during ${label} was $(trim_file "${ARTIFACT_DIR}/${label}.stdout"), want ${expected}"
	fi
}

verify_guest_file_missing() {
	local label="$1"
	local path="$2"

	if ! guest_ssh_capture "${label}" "if [ -e '${path}' ]; then echo present; else echo absent; fi"; then
		fail "guest file presence check for ${path} failed during ${label}"
	fi
	if [[ "$(trim_file "${ARTIFACT_DIR}/${label}.stdout")" != "absent" ]]; then
		fail "guest file ${path} should be absent during ${label}"
	fi
}

assert_stopped_state() {
	local label="$1"

	if ! srv_ssh_capture "${label}" inspect "${INSTANCE_NAME}"; then
		fail "inspect during ${label} failed"
	fi
	if ! grep -q '^state: stopped$' "${ARTIFACT_DIR}/${label}.stdout"; then
		fail "inspect during ${label} did not report state: stopped"
	fi
	if ! grep -q '^firecracker-pid: 0$' "${ARTIFACT_DIR}/${label}.stdout"; then
		fail "inspect during ${label} did not report firecracker-pid: 0"
	fi
	INSTANCE_TAP_DEVICE="$(extract_field "${ARTIFACT_DIR}/${label}.stdout" tap-device || true)"
}

assert_backup_artifacts() {
	local label="$1"

	if [[ -z "${BACKUP_ID}" || -z "${BACKUP_PATH}" ]]; then
		fail "backup metadata missing during ${label}"
	fi
	if [[ "${BACKUP_PATH}" != "${SRV_DATA_DIR}/backups/${INSTANCE_NAME}/${BACKUP_ID}" ]]; then
		fail "backup path ${BACKUP_PATH} did not match expected path ${SRV_DATA_DIR}/backups/${INSTANCE_NAME}/${BACKUP_ID} during ${label}"
	fi
	if [[ ! -d "${BACKUP_PATH}" ]]; then
		fail "backup path ${BACKUP_PATH} is missing during ${label}"
	fi
	for path in \
		"${BACKUP_PATH}/manifest.json" \
		"${BACKUP_PATH}/rootfs.img" \
		"${BACKUP_PATH}/serial.log" \
		"${BACKUP_PATH}/firecracker.log"; do
		if [[ ! -e "${path}" ]]; then
			fail "expected backup artifact ${path} is missing during ${label}"
		fi
	done
}

assert_host_runtime() {
	local stage="$1"
	local vm_runner_cgroup
	local cgroup_path
	local expected_cpu_max
	local pool_path

	vm_runner_cgroup="$(systemctl show -p ControlGroup --value srv-vm-runner.service)"
	if [[ -z "${vm_runner_cgroup}" ]]; then
		fail "srv-vm-runner.service did not report a control group during ${stage}"
	fi
	expected_cpu_max="$(( VM_VCPUS * 100000 )) 100000"
	if [[ "${CURRENT_MEMORY_MODE}" == "pool" ]]; then
		pool_path="/sys/fs/cgroup/${vm_runner_cgroup#/}/firecracker-pools/${CURRENT_POOL_NAME}"
		cgroup_path="${pool_path}/${INSTANCE_NAME}"
		if [[ ! -d "${pool_path}" ]]; then
			fail "pooled firecracker parent cgroup ${pool_path} is missing during ${stage}"
		fi
		if [[ ! -d "${cgroup_path}" ]]; then
			fail "pooled firecracker cgroup ${cgroup_path} is missing during ${stage}"
		fi
		if [[ "$(trim_file "${pool_path}/memory.max")" != "${POOL_SIZE_BYTES}" ]]; then
			fail "memory.max for ${pool_path} was $(trim_file "${pool_path}/memory.max"), want ${POOL_SIZE_BYTES} during ${stage}"
		fi
		if [[ "$(trim_file "${pool_path}/memory.swap.max")" != "0" ]]; then
			fail "memory.swap.max for ${pool_path} was $(trim_file "${pool_path}/memory.swap.max"), want 0 during ${stage}"
		fi
		if [[ "$(trim_file "${cgroup_path}/cpu.max")" != "${expected_cpu_max}" ]]; then
			fail "cpu.max for ${cgroup_path} was $(trim_file "${cgroup_path}/cpu.max"), want ${expected_cpu_max} during ${stage}"
		fi
		if [[ "$(trim_file "${cgroup_path}/memory.max")" != "max" ]]; then
			fail "memory.max for ${cgroup_path} was $(trim_file "${cgroup_path}/memory.max"), want max during ${stage}"
		fi
		if [[ "$(trim_file "${cgroup_path}/pids.max")" != "${VM_PIDS_MAX}" ]]; then
			fail "pids.max for ${cgroup_path} was $(trim_file "${cgroup_path}/pids.max"), want ${VM_PIDS_MAX} during ${stage}"
		fi
		return 0
	fi
	cgroup_path="/sys/fs/cgroup/${vm_runner_cgroup#/}/firecracker-vms/${INSTANCE_NAME}"
	if [[ ! -d "${cgroup_path}" ]]; then
		fail "firecracker cgroup ${cgroup_path} is missing during ${stage}"
	fi
	if [[ "$(trim_file "${cgroup_path}/cpu.max")" != "${expected_cpu_max}" ]]; then
		fail "cpu.max for ${cgroup_path} was $(trim_file "${cgroup_path}/cpu.max"), want ${expected_cpu_max} during ${stage}"
	fi
	if [[ "$(trim_file "${cgroup_path}/memory.max")" != "$(( CURRENT_MEMORY_MIB * 1024 * 1024 ))" ]]; then
		fail "memory.max for ${cgroup_path} was $(trim_file "${cgroup_path}/memory.max"), want $(( CURRENT_MEMORY_MIB * 1024 * 1024 )) during ${stage}"
	fi
	if [[ "$(trim_file "${cgroup_path}/memory.swap.max")" != "0" ]]; then
		fail "memory.swap.max for ${cgroup_path} was $(trim_file "${cgroup_path}/memory.swap.max"), want 0 during ${stage}"
	fi
	if [[ "$(trim_file "${cgroup_path}/pids.max")" != "${VM_PIDS_MAX}" ]]; then
		fail "pids.max for ${cgroup_path} was $(trim_file "${cgroup_path}/pids.max"), want ${VM_PIDS_MAX} during ${stage}"
	fi
}

assert_host_cleanup() {
	local stage="$1"
	local vm_runner_cgroup
	local cgroup_path
	local pool_path

	if [[ -n "${INSTANCE_TAP_DEVICE}" && -e "/sys/class/net/${INSTANCE_TAP_DEVICE}" ]]; then
		fail "tap device ${INSTANCE_TAP_DEVICE} still exists after ${stage}"
	fi
	if [[ -e "${JAILER_WORKSPACE_DIR}" ]]; then
		fail "jailer workspace ${JAILER_WORKSPACE_DIR} still exists after ${stage}"
	fi
	vm_runner_cgroup="$(systemctl show -p ControlGroup --value srv-vm-runner.service)"
	if [[ -n "${vm_runner_cgroup}" ]]; then
		if [[ "${CURRENT_MEMORY_MODE}" == "pool" ]]; then
			pool_path="/sys/fs/cgroup/${vm_runner_cgroup#/}/firecracker-pools/${CURRENT_POOL_NAME}"
			cgroup_path="${pool_path}/${INSTANCE_NAME}"
			if [[ -e "${cgroup_path}" ]]; then
				fail "pooled firecracker cgroup ${cgroup_path} still exists after ${stage}"
			fi
			if [[ ! -d "${pool_path}" ]]; then
				fail "pooled firecracker parent cgroup ${pool_path} is missing after ${stage}; shared pool parents should persist until pool delete"
			fi
			return 0
		fi
		cgroup_path="/sys/fs/cgroup/${vm_runner_cgroup#/}/firecracker-vms/${INSTANCE_NAME}"
		if [[ -e "${cgroup_path}" ]]; then
			fail "firecracker cgroup ${cgroup_path} still exists after ${stage}"
		fi
	fi
}

assert_pool_cleanup() {
	local stage="$1"
	local vm_runner_cgroup
	local pool_path

	vm_runner_cgroup="$(systemctl show -p ControlGroup --value srv-vm-runner.service)"
	if [[ -z "${vm_runner_cgroup}" ]]; then
		fail "srv-vm-runner.service did not report a control group during ${stage}"
	fi
	pool_path="/sys/fs/cgroup/${vm_runner_cgroup#/}/firecracker-pools/${POOL_NAME}"
	if [[ -e "${pool_path}" ]]; then
		fail "pooled firecracker parent cgroup ${pool_path} still exists after ${stage}"
	fi
}

assert_pool_present() {
	local label="$1"
	if ! srv_ssh_capture "${label}" pool inspect "${POOL_NAME}"; then
		fail "pool inspect ${POOL_NAME} failed during ${label}"
	fi
}

assert_pool_members() {
	local label="$1"
	local expected_members="$2"
	shift 2
	local expected_member_name
	assert_pool_present "${label}"
	if [[ "$(extract_field "${ARTIFACT_DIR}/${label}.stdout" members || true)" != "${expected_members}" ]]; then
		fail "pool inspect during ${label} reported members $(extract_field "${ARTIFACT_DIR}/${label}.stdout" members || true), want ${expected_members}"
	fi
	for expected_member_name in "$@"; do
		if ! grep -q "^- ${expected_member_name} (" "${ARTIFACT_DIR}/${label}.stdout"; then
			fail "pool inspect during ${label} did not list member ${expected_member_name}"
		fi
	done
}

assert_pool_delete_rejected() {
	local label="$1"
	if srv_ssh_capture "${label}" pool delete "${POOL_NAME}"; then
		fail "pool delete ${POOL_NAME} unexpectedly succeeded during ${label}"
	fi
	if ! grep -q "still has" "${ARTIFACT_DIR}/${label}.stderr"; then
		fail "pool delete ${POOL_NAME} during ${label} did not explain that the pool still has members"
	fi
}

assert_pooled_inspect() {
	local label="$1"
	if ! srv_ssh_capture "${label}" inspect "${INSTANCE_NAME}"; then
		fail "inspect ${INSTANCE_NAME} failed during ${label}"
	fi
	for want in \
		"memory: ${CURRENT_MEMORY_MIB} MiB" \
		"memory-mode: pool" \
		"memory-pool: ${POOL_NAME}" \
		"host-reservation: shared via pool"; do
		if ! grep -q "^${want}$" "${ARTIFACT_DIR}/${label}.stdout"; then
			fail "inspect ${INSTANCE_NAME} during ${label} did not report ${want}"
		fi
	done
}

assert_pooled_balloon() {
	local label="$1"
	local deadline=$(( $(date -u +%s) + GUEST_SSH_READY_TIMEOUT ))
	local socket_path="${INSTANCE_DIR}/firecracker.sock"
	local amount_mib
	local target_mib
	local actual_mib
	while :; do
		if run_capture "${label}-balloon" curl --silent --show-error --fail --unix-socket "${socket_path}" http://localhost/balloon; then
			if run_capture "${label}-balloon-stats" curl --silent --show-error --fail --unix-socket "${socket_path}" http://localhost/balloon/statistics; then
				break
			fi
		fi
		if [[ "$(date -u +%s)" -ge "${deadline}" ]]; then
			fail "balloon API never became available for ${INSTANCE_NAME} during ${label}"
		fi
		sleep "${POLL_INTERVAL_SECONDS}"
	done
	for want in '"deflate_on_oom":true' '"stats_polling_interval_s":5' '"free_page_reporting":true'; do
		if ! tr -d '[:space:]' <"${ARTIFACT_DIR}/${label}-balloon.stdout" | grep -q "${want}"; then
			fail "balloon config for ${INSTANCE_NAME} during ${label} did not report ${want}"
		fi
	done
	amount_mib="$(json_number_for_key "${ARTIFACT_DIR}/${label}-balloon.stdout" amount_mib || true)"
	target_mib="$(json_number_for_key "${ARTIFACT_DIR}/${label}-balloon-stats.stdout" target_mib || true)"
	actual_mib="$(json_number_for_key "${ARTIFACT_DIR}/${label}-balloon-stats.stdout" actual_mib || true)"
	if [[ -z "${amount_mib}" ]]; then
		fail "balloon config for ${INSTANCE_NAME} during ${label} did not report a numeric amount_mib"
	fi
	if [[ -z "${target_mib}" ]]; then
		fail "balloon statistics for ${INSTANCE_NAME} during ${label} did not report a numeric target_mib"
	fi
	if [[ -z "${actual_mib}" ]]; then
		fail "balloon statistics for ${INSTANCE_NAME} during ${label} did not report a numeric actual_mib"
	fi
}

assert_pooled_balloon_reclaims_cache() {
	local label="$1"
	local cache_probe_path="/var/tmp/srv-balloon-cache-probe"
	local socket_path="${INSTANCE_DIR}/firecracker.sock"
	local latest_label="${label}-latest"
	local deadline=$(( $(date -u +%s) + GUEST_SSH_READY_TIMEOUT ))
	local target_mib

	if ! guest_ssh_capture "${label}-seed-cache" "dd if=/dev/zero of='${cache_probe_path}' bs=1M count=256 conv=fsync >/dev/null 2>&1 && sync"; then
		fail "failed to seed guest page cache for balloon reclaim during ${label}"
	fi
	while :; do
		if run_capture "${latest_label}" curl --silent --show-error --fail --unix-socket "${socket_path}" http://localhost/balloon/statistics; then
			target_mib="$(json_number_for_key "${ARTIFACT_DIR}/${latest_label}.stdout" target_mib || true)"
			if [[ -n "${target_mib}" && "${target_mib}" -gt 0 ]]; then
				copy_capture_result "${latest_label}" "${label}"
				guest_ssh_capture "${label}-cleanup-cache" "rm -f '${cache_probe_path}' && sync" || true
				return 0
			fi
		fi
		if [[ "$(date -u +%s)" -ge "${deadline}" ]]; then
			guest_ssh_capture "${label}-cleanup-cache-timeout" "rm -f '${cache_probe_path}' && sync" || true
			fail "balloon reclaim loop never raised target_mib above 0 for ${INSTANCE_NAME} during ${label}"
		fi
		sleep "${POLL_INTERVAL_SECONDS}"
	done
}

capture_diagnostics() {
	log "capturing diagnostics in ${ARTIFACT_DIR}"
	capture_best_effort systemctl-status systemctl status srv srv-net-helper srv-vm-runner --no-pager --full
	capture_best_effort journalctl-services journalctl --no-pager --since "${RUN_STARTED_AT}" -u srv -u srv-net-helper -u srv-vm-runner
	capture_best_effort tailscale-status tailscale status
	capture_best_effort_ssh srv-list list
	if (( POOL_ATTEMPTED )); then
		capture_best_effort_ssh pool-inspect-final pool inspect "${POOL_NAME}"
		capture_best_effort_ssh pool-list-final pool list
	fi
	if (( INSTANCE_ATTEMPTED )); then
		capture_best_effort_ssh inspect-final inspect "${INSTANCE_NAME}"
		capture_best_effort_ssh logs-serial logs "${INSTANCE_NAME}" serial
		capture_best_effort_ssh logs-firecracker logs "${INSTANCE_NAME}" firecracker
		capture_best_effort instance-dir ls -la "${INSTANCE_DIR}"
	fi
}

cleanup_instance() {
	if (( CLEANUP_COMPLETE )) || (( ! INSTANCE_ATTEMPTED && ${#CLEANUP_INSTANCE_NAMES[@]} == 0 )); then
		return 0
	fi
	if [[ "${KEEP_FAILED}" == "1" ]]; then
		log "KEEP_FAILED=1; leaving attempted instances intact"
		return 0
	fi
	if (( ${#CLEANUP_INSTANCE_NAMES[@]} == 0 )); then
		CLEANUP_INSTANCE_NAMES+=("${INSTANCE_NAME}")
	fi
	local name
	for name in "${CLEANUP_INSTANCE_NAMES[@]}"; do
		log "cleaning up ${name}"
		capture_best_effort_ssh "cleanup-delete-${name}" delete "${name}"
	done
}

cleanup_pool() {
	if (( POOL_CLEANUP_COMPLETE )) || (( ! POOL_ATTEMPTED )); then
		return 0
	fi
	if [[ "${KEEP_FAILED}" == "1" ]]; then
		log "KEEP_FAILED=1; leaving pool ${POOL_NAME} intact"
		return 0
	fi
	log "cleaning up pool ${POOL_NAME}"
	capture_best_effort_ssh cleanup-pool-delete pool delete "${POOL_NAME}"
}

on_exit() {
	local rc="$1"
	if [[ "${rc}" -ne 0 ]]; then
		capture_diagnostics
		cleanup_instance
		cleanup_pool
		log "smoke test failed; artifacts: ${ARTIFACT_DIR}"
		return
	fi
	log "smoke test passed; artifacts: ${ARTIFACT_DIR}"
}

require_root
require_commands
load_env

VM_VCPUS="${SRV_VM_VCPUS:-1}"
VM_MEMORY_MIB="${SRV_VM_MEMORY_MIB:-1024}"
VM_PIDS_MAX="${SRV_VM_PIDS_MAX:-512}"
POOL_SIZE_MIB="${POOL_SIZE_MIB:-2048}"
POOLED_VM_MEMORY_MIB="${POOLED_VM_MEMORY_MIB:-1024}"
POOLED_VM_RESIZE_MIB="${POOLED_VM_RESIZE_MIB:-1536}"

CONTROL_HOST="${SMOKE_SSH_HOST:-${SRV_HOSTNAME:-srv}}"
SRV_DATA_DIR="${SRV_DATA_DIR:-/var/lib/srv}"
BASE_KERNEL_PATH="${SRV_BASE_KERNEL:-}"
BASE_ROOTFS_PATH="${SRV_BASE_ROOTFS:-}"
FIRECRACKER_BIN_PATH="${SRV_FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BASE_DIR="${SRV_JAILER_BASE_DIR:-${SRV_DATA_DIR}/jailer}"
JAILER_EXEC_NAME="$(basename "${FIRECRACKER_BIN_PATH}")"
BASE_INSTANCE_NAME="${INSTANCE_NAME:-smoke-$(date -u +%Y%m%d%H%M%S)-$$}"
POOL_NAME="${POOL_NAME:-${BASE_INSTANCE_NAME}-pool}"
POOLED_INSTANCE_NAME="${POOLED_INSTANCE_NAME:-${BASE_INSTANCE_NAME}-pooled}"
POOLED_PEER_INSTANCE_NAME="${POOLED_PEER_INSTANCE_NAME:-${BASE_INSTANCE_NAME}-pooled-peer}"
POOL_SIZE_BYTES=$(( POOL_SIZE_MIB * 1024 * 1024 ))
POOL_SIZE_ARG="${POOL_SIZE_MIB}MiB"
POOLED_VM_MEMORY_ARG="${POOLED_VM_MEMORY_MIB}MiB"
POOLED_VM_RESIZE_ARG="${POOLED_VM_RESIZE_MIB}MiB"
POOLED_VM_OVERSIZE_MIB="${POOLED_VM_OVERSIZE_MIB:-$(( POOL_SIZE_MIB + 512 ))}"
POOLED_VM_OVERSIZE_ARG="${POOLED_VM_OVERSIZE_MIB}MiB"
INSTANCE_NAME="${BASE_INSTANCE_NAME}"
ARTIFACT_DIR="${ARTIFACT_DIR:-${ARTIFACT_ROOT}/${INSTANCE_NAME}}"
READY_TIMEOUT_SECONDS="$(derive_ready_timeout_seconds)"
READY_TIMEOUT_SECONDS=$(( READY_TIMEOUT_SECONDS + READY_TIMEOUT_BUFFER_SECONDS ))

if (( POOL_SIZE_MIB <= 0 || POOLED_VM_MEMORY_MIB <= 0 || POOLED_VM_RESIZE_MIB <= 0 || POOLED_VM_OVERSIZE_MIB <= 0 )); then
	fail "pool and pooled VM memory sizes must be positive"
fi

if (( POOLED_VM_MEMORY_MIB > POOL_SIZE_MIB )); then
	fail "POOLED_VM_MEMORY_MIB must not exceed POOL_SIZE_MIB"
fi

if (( POOLED_VM_RESIZE_MIB > POOL_SIZE_MIB )); then
	fail "POOLED_VM_RESIZE_MIB must not exceed POOL_SIZE_MIB"
fi

set_active_instance "${BASE_INSTANCE_NAME}"
CURRENT_MEMORY_MODE="fixed"
CURRENT_POOL_NAME=""
CURRENT_MEMORY_MIB="${VM_MEMORY_MIB}"

mkdir -p "${ARTIFACT_DIR}"
trap 'on_exit "$?"' EXIT

SSH_KNOWN_HOSTS="${ARTIFACT_DIR}/known_hosts"
ssh_opts=(
	-o BatchMode=yes
	-o ConnectTimeout="${SSH_CONNECT_TIMEOUT}"
	-o StrictHostKeyChecking=accept-new
	-o UserKnownHostsFile="${SSH_KNOWN_HOSTS}"
)

cat >"${ARTIFACT_DIR}/context.txt" <<EOF
run-started-at: ${RUN_STARTED_AT}
run-started-epoch: ${RUN_STARTED_EPOCH}
control-host: ${CONTROL_HOST}
ssh-user: ${SSH_USER}
instance-name: ${INSTANCE_NAME}
env-path: ${ENV_PATH}
srv-data-dir: ${SRV_DATA_DIR}
ready-timeout-seconds: ${READY_TIMEOUT_SECONDS}
poll-interval-seconds: ${POLL_INTERVAL_SECONDS}
guest-ssh-ready-timeout: ${GUEST_SSH_READY_TIMEOUT}
keep-failed: ${KEEP_FAILED}
jailer-workspace-dir: ${JAILER_WORKSPACE_DIR}
pool-name: ${POOL_NAME}
pooled-instance-name: ${POOLED_INSTANCE_NAME}
pooled-peer-instance-name: ${POOLED_PEER_INSTANCE_NAME}
pool-size-mib: ${POOL_SIZE_MIB}
pooled-vm-memory-mib: ${POOLED_VM_MEMORY_MIB}
pooled-vm-resize-mib: ${POOLED_VM_RESIZE_MIB}
EOF

if [[ ! -c /dev/kvm ]]; then
	fail "/dev/kvm is required"
fi

if [[ -z "${BASE_KERNEL_PATH}" || ! -f "${BASE_KERNEL_PATH}" ]]; then
	fail "base kernel is missing; set SRV_BASE_KERNEL in ${ENV_PATH}"
fi

if [[ -z "${BASE_ROOTFS_PATH}" || ! -f "${BASE_ROOTFS_PATH}" ]]; then
	fail "base rootfs is missing; set SRV_BASE_ROOTFS in ${ENV_PATH}"
fi

if [[ ! -d "${SRV_DATA_DIR}" ]]; then
	fail "SRV_DATA_DIR ${SRV_DATA_DIR} does not exist"
fi

for unit in srv-net-helper.service srv-vm-runner.service srv.service; do
	if ! systemctl is-active --quiet "${unit}"; then
		fail "systemd unit ${unit} is not active"
	fi
done

log "verifying SSH control plane reachability"
if ! srv_ssh_capture preflight-help help; then
	fail "ssh ${CONTROL_HOST} help failed"
fi

log "creating ${INSTANCE_NAME}"
INSTANCE_ATTEMPTED=1
register_cleanup_instance "${INSTANCE_NAME}"
if ! srv_ssh_capture create new "${INSTANCE_NAME}"; then
	log "create command failed; inspecting instance state before deciding"
fi

wait_for_ready inspect-create-ready

log "instance is ready as ${TAILSCALE_NAME} (${TAILSCALE_IP})"
capture_best_effort tailscale-status-after-ready tailscale status
verify_guest_ssh guest-ssh-create-ready
if ! guest_ssh_capture guest-marker-before-backup "printf '%s\n' 'before-backup' > '${BACKUP_MARKER_PATH}' && sync"; then
	fail "failed to create backup marker before stopped backup"
fi
verify_guest_file_content guest-marker-before-backup-verify "${BACKUP_MARKER_PATH}" "before-backup"
assert_instance_listed list-create-ready ready
assert_host_runtime create-ready

log "stopping ${INSTANCE_NAME}"
if ! srv_ssh_capture stop stop "${INSTANCE_NAME}"; then
	fail "stop ${INSTANCE_NAME} failed"
fi
if ! grep -q '^state: stopped$' "${ARTIFACT_DIR}/stop.stdout"; then
	fail "stop ${INSTANCE_NAME} did not report state: stopped"
fi
assert_stopped_state inspect-stopped
assert_host_cleanup stop

log "creating stopped backup for ${INSTANCE_NAME}"
if ! srv_ssh_capture backup-create backup create "${INSTANCE_NAME}"; then
	fail "backup create ${INSTANCE_NAME} failed"
fi
BACKUP_ID="$(extract_field "${ARTIFACT_DIR}/backup-create.stdout" backup-id || true)"
BACKUP_PATH="$(extract_field "${ARTIFACT_DIR}/backup-create.stdout" path || true)"
if [[ -z "${BACKUP_ID}" ]]; then
	fail "backup create did not report a backup-id"
fi
if ! grep -q "^backup-created: ${INSTANCE_NAME}$" "${ARTIFACT_DIR}/backup-create.stdout"; then
	fail "backup create did not report backup-created: ${INSTANCE_NAME}"
fi
assert_backup_artifacts backup-create

log "listing backups for ${INSTANCE_NAME}"
if ! srv_ssh_capture backup-list backup list "${INSTANCE_NAME}"; then
	fail "backup list ${INSTANCE_NAME} failed"
fi
if ! grep -q "${BACKUP_ID}" "${ARTIFACT_DIR}/backup-list.stdout"; then
	fail "backup list did not include backup ${BACKUP_ID}"
fi

log "starting ${INSTANCE_NAME}"
if ! srv_ssh_capture start start "${INSTANCE_NAME}"; then
	fail "start ${INSTANCE_NAME} failed"
fi

wait_for_ready inspect-start-ready

log "instance is ready again as ${TAILSCALE_NAME} (${TAILSCALE_IP})"
capture_best_effort tailscale-status-after-restart tailscale status
verify_guest_ssh guest-ssh-start-ready
if ! guest_ssh_capture guest-marker-after-backup "printf '%s\n' 'after-backup' > '${BACKUP_MARKER_PATH}' && printf '%s\n' 'post-backup-only' > '${POST_BACKUP_PATH}' && sync"; then
	fail "failed to mutate guest after backup"
fi
verify_guest_file_content guest-marker-after-backup-verify "${BACKUP_MARKER_PATH}" "after-backup"
verify_guest_file_content guest-post-backup-file-verify "${POST_BACKUP_PATH}" "post-backup-only"
assert_instance_listed list-start-ready ready
assert_host_runtime start-ready

log "stopping ${INSTANCE_NAME} for restore"
if ! srv_ssh_capture stop-before-restore stop "${INSTANCE_NAME}"; then
	fail "stop ${INSTANCE_NAME} before restore failed"
fi
if ! grep -q '^state: stopped$' "${ARTIFACT_DIR}/stop-before-restore.stdout"; then
	fail "stop before restore did not report state: stopped"
fi
assert_stopped_state inspect-before-restore
assert_host_cleanup stop-before-restore

log "restoring ${INSTANCE_NAME} from backup ${BACKUP_ID}"
if ! srv_ssh_capture restore restore "${INSTANCE_NAME}" "${BACKUP_ID}"; then
	fail "restore ${INSTANCE_NAME} from backup ${BACKUP_ID} failed"
fi
if ! grep -q "^backup-id: ${BACKUP_ID}$" "${ARTIFACT_DIR}/restore.stdout"; then
	fail "restore did not report backup-id: ${BACKUP_ID}"
fi
if ! grep -q '^state: stopped$' "${ARTIFACT_DIR}/restore.stdout"; then
	fail "restore did not report state: stopped"
fi
assert_stopped_state inspect-restored-stopped

log "starting ${INSTANCE_NAME} after restore"
if ! srv_ssh_capture start-after-restore start "${INSTANCE_NAME}"; then
	fail "start ${INSTANCE_NAME} after restore failed"
fi

wait_for_ready inspect-restore-ready

log "instance is ready after restore as ${TAILSCALE_NAME} (${TAILSCALE_IP})"
capture_best_effort tailscale-status-after-restore tailscale status
verify_guest_ssh guest-ssh-restore-ready
verify_guest_file_content guest-marker-restored "${BACKUP_MARKER_PATH}" "before-backup"
verify_guest_file_missing guest-post-backup-file-restored "${POST_BACKUP_PATH}"
assert_instance_listed list-restore-ready ready
assert_host_runtime restore-ready

log "deleting ${INSTANCE_NAME}"
if ! srv_ssh_capture delete delete "${INSTANCE_NAME}"; then
	fail "delete ${INSTANCE_NAME} failed"
fi
if ! grep -q '^state: deleted$' "${ARTIFACT_DIR}/delete.stdout"; then
	fail "delete ${INSTANCE_NAME} did not report state: deleted"
fi

log "confirming teardown"
if ! srv_ssh_capture list-after-delete list; then
	fail "list after delete failed"
fi
if list_output_contains "${ARTIFACT_DIR}/list-after-delete.stdout" "${INSTANCE_NAME}"; then
	fail "instance ${INSTANCE_NAME} still appears in list output after delete"
fi
if [[ -e "${INSTANCE_DIR}" ]]; then
	fail "instance directory ${INSTANCE_DIR} still exists after delete"
fi
assert_host_cleanup delete

CLEANUP_COMPLETE=1

log "checking status before pooled memory pool create"
if ! srv_ssh_capture status-before-pool-create status; then
	fail "status before pool create failed"
fi
STATUS_BEFORE_POOL_CREATE_POOLS="$(box_value_for_label "${ARTIFACT_DIR}/status-before-pool-create.stdout" POOLS)"
if [[ -z "${STATUS_BEFORE_POOL_CREATE_POOLS}" ]]; then
	fail "status before pool create did not expose a POOLS row"
fi

log "creating pooled memory pool ${POOL_NAME}"
POOL_ATTEMPTED=1
if ! srv_ssh_capture pool-create pool create "${POOL_NAME}" --size "${POOL_SIZE_ARG}"; then
	fail "pool create ${POOL_NAME} failed"
fi
if ! grep -q "^pool-created: ${POOL_NAME}$" "${ARTIFACT_DIR}/pool-create.stdout"; then
	fail "pool create ${POOL_NAME} did not report pool-created: ${POOL_NAME}"
fi
if ! grep -q '^reserved: ' "${ARTIFACT_DIR}/pool-create.stdout"; then
	fail "pool create ${POOL_NAME} did not report reserved capacity"
fi
assert_pool_members pool-inspect-created 0

log "checking pool reservation shows up in status before any pooled VM exists"
if ! srv_ssh_capture status-after-pool-create status; then
	fail "status after pool create failed"
fi
STATUS_AFTER_POOL_CREATE_MEMORY="$(box_value_for_label "${ARTIFACT_DIR}/status-after-pool-create.stdout" MEMORY)"
STATUS_AFTER_POOL_CREATE_POOLS="$(box_value_for_label "${ARTIFACT_DIR}/status-after-pool-create.stdout" POOLS)"
if [[ -z "${STATUS_AFTER_POOL_CREATE_MEMORY}" || -z "${STATUS_AFTER_POOL_CREATE_POOLS}" ]]; then
	fail "status after pool create did not expose MEMORY and POOLS rows"
fi
if [[ "${STATUS_AFTER_POOL_CREATE_POOLS}" == "${STATUS_BEFORE_POOL_CREATE_POOLS}" ]]; then
	fail "status pools line did not change after creating pool ${POOL_NAME}; pool reservation should be reflected in status"
fi

log "creating pooled instance ${POOLED_INSTANCE_NAME}"
set_active_instance "${POOLED_INSTANCE_NAME}"
INSTANCE_ATTEMPTED=1
CLEANUP_COMPLETE=0
register_cleanup_instance "${INSTANCE_NAME}"
CURRENT_MEMORY_MODE="pool"
CURRENT_POOL_NAME="${POOL_NAME}"
CURRENT_MEMORY_MIB="${POOLED_VM_MEMORY_MIB}"
if ! srv_ssh_capture pooled-create new "${INSTANCE_NAME}" --pool "${POOL_NAME}" --ram "${POOLED_VM_MEMORY_ARG}"; then
	fail "create pooled instance ${INSTANCE_NAME} failed"
fi

wait_for_ready inspect-pooled-create-ready

log "pooled instance is ready as ${TAILSCALE_NAME} (${TAILSCALE_IP})"
verify_guest_ssh guest-ssh-pooled-ready
assert_instance_listed list-pooled-ready ready
assert_pooled_inspect inspect-pooled-ready
assert_host_runtime pooled-create-ready
assert_pooled_balloon pooled-create-ready
assert_pool_members pool-inspect-with-member 1 "${INSTANCE_NAME}"

log "checking pooled instance does not add a second host memory reservation"
if ! srv_ssh_capture status-with-pooled-member status; then
	fail "status with pooled member failed"
fi
if [[ "$(box_value_for_label "${ARTIFACT_DIR}/status-with-pooled-member.stdout" MEMORY)" != "${STATUS_AFTER_POOL_CREATE_MEMORY}" ]]; then
	fail "status memory line changed after creating pooled member; pooled VMs should not add host reservation again"
fi
if [[ "$(box_value_for_label "${ARTIFACT_DIR}/status-with-pooled-member.stdout" POOLS)" != "${STATUS_AFTER_POOL_CREATE_POOLS}" ]]; then
	fail "status pools line changed after creating pooled member; pooled VMs should not change pool reservation accounting"
fi

log "creating peer pooled instance ${POOLED_PEER_INSTANCE_NAME}"
set_active_instance "${POOLED_PEER_INSTANCE_NAME}"
register_cleanup_instance "${INSTANCE_NAME}"
CURRENT_MEMORY_MIB="${POOLED_VM_MEMORY_MIB}"
if ! srv_ssh_capture pooled-peer-create new "${INSTANCE_NAME}" --pool "${POOL_NAME}" --ram "${POOLED_VM_MEMORY_ARG}"; then
	fail "create peer pooled instance ${INSTANCE_NAME} failed"
fi

wait_for_ready inspect-pooled-peer-create-ready

log "peer pooled instance is ready as ${TAILSCALE_NAME} (${TAILSCALE_IP})"
verify_guest_ssh guest-ssh-pooled-peer-ready
assert_instance_listed list-pooled-peer-ready ready
assert_pooled_inspect inspect-pooled-peer-ready
assert_host_runtime pooled-peer-create-ready
assert_pooled_balloon pooled-peer-create-ready
assert_pool_members pool-inspect-with-two-members 2 "${POOLED_INSTANCE_NAME}" "${POOLED_PEER_INSTANCE_NAME}"
assert_pool_delete_rejected pool-delete-non-empty

log "checking two pooled instances still share one host memory reservation"
if ! srv_ssh_capture status-with-two-pooled-members status; then
	fail "status with two pooled members failed"
fi
if [[ "$(box_value_for_label "${ARTIFACT_DIR}/status-with-two-pooled-members.stdout" MEMORY)" != "${STATUS_AFTER_POOL_CREATE_MEMORY}" ]]; then
	fail "status memory line changed after creating second pooled member; pooled VMs should not add host reservation again"
fi
if [[ "$(box_value_for_label "${ARTIFACT_DIR}/status-with-two-pooled-members.stdout" POOLS)" != "${STATUS_AFTER_POOL_CREATE_POOLS}" ]]; then
	fail "status pools line changed after creating second pooled member; pooled VMs should not change pool reservation accounting"
fi

log "checking balloon reclaim for both pooled instances"
assert_pooled_balloon_reclaims_cache pooled-peer-reclaim
set_active_instance "${POOLED_INSTANCE_NAME}"
CURRENT_MEMORY_MIB="${POOLED_VM_MEMORY_MIB}"
wait_for_ready inspect-pooled-primary-ready-before-reclaim
assert_pooled_balloon_reclaims_cache pooled-primary-reclaim-with-peer

log "deleting peer pooled instance ${POOLED_PEER_INSTANCE_NAME}"
set_active_instance "${POOLED_PEER_INSTANCE_NAME}"
CURRENT_MEMORY_MIB="${POOLED_VM_MEMORY_MIB}"
if ! srv_ssh_capture pooled-peer-delete delete "${INSTANCE_NAME}"; then
	fail "delete peer pooled instance ${INSTANCE_NAME} failed"
fi
if ! grep -q '^state: deleted$' "${ARTIFACT_DIR}/pooled-peer-delete.stdout"; then
	fail "delete peer pooled instance ${INSTANCE_NAME} did not report state: deleted"
fi
assert_host_cleanup pooled-peer-delete
assert_pool_members pool-inspect-after-peer-delete 1 "${POOLED_INSTANCE_NAME}"

set_active_instance "${POOLED_INSTANCE_NAME}"
CURRENT_MEMORY_MIB="${POOLED_VM_MEMORY_MIB}"

log "stopping pooled instance ${INSTANCE_NAME} for resize checks"
if ! srv_ssh_capture pooled-stop stop "${INSTANCE_NAME}"; then
	fail "stop pooled instance ${INSTANCE_NAME} failed"
fi
if ! grep -q '^state: stopped$' "${ARTIFACT_DIR}/pooled-stop.stdout"; then
	fail "stop pooled instance ${INSTANCE_NAME} did not report state: stopped"
fi
assert_stopped_state inspect-pooled-stopped
assert_host_cleanup pooled-stop

log "resizing pooled instance ${INSTANCE_NAME} within pool reservation"
if ! srv_ssh_capture pooled-resize-ok resize "${INSTANCE_NAME}" --ram "${POOLED_VM_RESIZE_ARG}"; then
	fail "resize pooled instance ${INSTANCE_NAME} within pool reservation failed"
fi
if ! grep -q "^memory: ${POOLED_VM_RESIZE_MIB} MiB$" "${ARTIFACT_DIR}/pooled-resize-ok.stdout"; then
	fail "resize pooled instance ${INSTANCE_NAME} did not report memory ${POOLED_VM_RESIZE_MIB} MiB"
fi
CURRENT_MEMORY_MIB="${POOLED_VM_RESIZE_MIB}"
assert_pooled_inspect inspect-pooled-resized-stopped

log "verifying pooled resize beyond pool reservation is rejected"
if srv_ssh_capture pooled-resize-too-large resize "${INSTANCE_NAME}" --ram "${POOLED_VM_OVERSIZE_ARG}"; then
	fail "resize pooled instance ${INSTANCE_NAME} beyond pool reservation unexpectedly succeeded"
fi
if ! grep -q "exceeds memory pool" "${ARTIFACT_DIR}/pooled-resize-too-large.stderr"; then
	fail "oversized pooled resize did not explain that it exceeds the pool reservation"
fi
assert_pooled_inspect inspect-pooled-resize-rejected

log "starting pooled instance ${INSTANCE_NAME} after resize"
if ! srv_ssh_capture pooled-start start "${INSTANCE_NAME}"; then
	fail "start pooled instance ${INSTANCE_NAME} after resize failed"
fi

wait_for_ready inspect-pooled-resize-ready

log "pooled instance is ready after resize as ${TAILSCALE_NAME} (${TAILSCALE_IP})"
verify_guest_ssh guest-ssh-pooled-resize-ready
assert_pooled_inspect inspect-pooled-resize-ready
assert_host_runtime pooled-resize-ready
assert_pooled_balloon pooled-resize-ready

log "deleting pooled instance ${INSTANCE_NAME}"
if ! srv_ssh_capture pooled-delete delete "${INSTANCE_NAME}"; then
	fail "delete pooled instance ${INSTANCE_NAME} failed"
fi
if ! grep -q '^state: deleted$' "${ARTIFACT_DIR}/pooled-delete.stdout"; then
	fail "delete pooled instance ${INSTANCE_NAME} did not report state: deleted"
fi
assert_host_cleanup pooled-delete
assert_pool_members pool-inspect-empty 0
CLEANUP_COMPLETE=1

log "deleting pool ${POOL_NAME}"
if ! srv_ssh_capture pool-delete pool delete "${POOL_NAME}"; then
	fail "pool delete ${POOL_NAME} failed"
fi
if ! grep -q "^pool-deleted: ${POOL_NAME}$" "${ARTIFACT_DIR}/pool-delete.stdout"; then
	fail "pool delete ${POOL_NAME} did not report pool-deleted: ${POOL_NAME}"
fi
POOL_CLEANUP_COMPLETE=1
assert_pool_cleanup pool-delete

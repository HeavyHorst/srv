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
  ARTIFACT_ROOT                 default: /var/tmp/srv-smoke
  ARTIFACT_DIR                  default: <ARTIFACT_ROOT>/<INSTANCE_NAME>
  KEEP_FAILED                   default: 0
  POLL_INTERVAL_SECONDS         default: 5
  READY_TIMEOUT_BUFFER_SECONDS  default: 30
  READY_TIMEOUT_SECONDS         default: derived from SRV_GUEST_READY_TIMEOUT
  GUEST_SSH_READY_TIMEOUT       default: 30
  SSH_CONNECT_TIMEOUT           default: 10
  STRICT_HOST_ASSERTIONS        default: 0
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
STRICT_HOST_ASSERTIONS="${STRICT_HOST_ASSERTIONS:-0}"

RUN_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RUN_STARTED_EPOCH="$(date -u +%s)"

INSTANCE_ATTEMPTED=0
CLEANUP_COMPLETE=0
TAILSCALE_NAME=""
TAILSCALE_IP=""
INSTANCE_TAP_DEVICE=""

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
	for cmd in awk cp date grep journalctl ls mkdir sed ssh systemctl tailscale; do
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

assert_instance_listed() {
	local label="$1"
	local expected_state="$2"
	if ! srv_ssh_capture "${label}" list; then
		fail "list failed during ${label}"
	fi
	if ! awk -v name="${INSTANCE_NAME}" -v state="${expected_state}" '$1 == name && $2 == state { found = 1 } END { exit(found ? 0 : 1) }' "${ARTIFACT_DIR}/${label}.stdout"; then
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

assert_strict_host_runtime() {
	local stage="$1"
	local vm_runner_cgroup
	local cgroup_path
	local expected_cpu_max
	local expected_memory_max

	if [[ "${STRICT_HOST_ASSERTIONS}" != "1" ]]; then
		return 0
	fi
	vm_runner_cgroup="$(systemctl show -p ControlGroup --value srv-vm-runner.service)"
	if [[ -z "${vm_runner_cgroup}" ]]; then
		fail "srv-vm-runner.service did not report a control group during ${stage}"
	fi
	cgroup_path="/sys/fs/cgroup/${vm_runner_cgroup#/}/firecracker-vms/${INSTANCE_NAME}"
	if [[ ! -d "${cgroup_path}" ]]; then
		fail "firecracker cgroup ${cgroup_path} is missing during ${stage}"
	fi
	expected_cpu_max="$(( VM_VCPUS * 100000 )) 100000"
	expected_memory_max="$(( VM_MEMORY_MIB * 1024 * 1024 ))"
	if [[ "$(trim_file "${cgroup_path}/cpu.max")" != "${expected_cpu_max}" ]]; then
		fail "cpu.max for ${cgroup_path} was $(trim_file "${cgroup_path}/cpu.max"), want ${expected_cpu_max} during ${stage}"
	fi
	if [[ "$(trim_file "${cgroup_path}/memory.max")" != "${expected_memory_max}" ]]; then
		fail "memory.max for ${cgroup_path} was $(trim_file "${cgroup_path}/memory.max"), want ${expected_memory_max} during ${stage}"
	fi
	if [[ "$(trim_file "${cgroup_path}/memory.swap.max")" != "0" ]]; then
		fail "memory.swap.max for ${cgroup_path} was $(trim_file "${cgroup_path}/memory.swap.max"), want 0 during ${stage}"
	fi
	if [[ "$(trim_file "${cgroup_path}/pids.max")" != "${VM_PIDS_MAX}" ]]; then
		fail "pids.max for ${cgroup_path} was $(trim_file "${cgroup_path}/pids.max"), want ${VM_PIDS_MAX} during ${stage}"
	fi
}

assert_strict_host_cleanup() {
	local stage="$1"
	local vm_runner_cgroup
	local cgroup_path

	if [[ "${STRICT_HOST_ASSERTIONS}" != "1" ]]; then
		return 0
	fi
	if [[ -n "${INSTANCE_TAP_DEVICE}" && -e "/sys/class/net/${INSTANCE_TAP_DEVICE}" ]]; then
		fail "tap device ${INSTANCE_TAP_DEVICE} still exists after ${stage}"
	fi
	if [[ -e "${JAILER_WORKSPACE_DIR}" ]]; then
		fail "jailer workspace ${JAILER_WORKSPACE_DIR} still exists after ${stage}"
	fi
	vm_runner_cgroup="$(systemctl show -p ControlGroup --value srv-vm-runner.service)"
	if [[ -n "${vm_runner_cgroup}" ]]; then
		cgroup_path="/sys/fs/cgroup/${vm_runner_cgroup#/}/firecracker-vms/${INSTANCE_NAME}"
		if [[ -e "${cgroup_path}" ]]; then
			fail "firecracker cgroup ${cgroup_path} still exists after ${stage}"
		fi
	fi
}

capture_diagnostics() {
	log "capturing diagnostics in ${ARTIFACT_DIR}"
	capture_best_effort systemctl-status systemctl status srv srv-net-helper srv-vm-runner --no-pager --full
	capture_best_effort journalctl-services journalctl --no-pager --since "${RUN_STARTED_AT}" -u srv -u srv-net-helper -u srv-vm-runner
	capture_best_effort tailscale-status tailscale status
	capture_best_effort_ssh srv-list list
	if (( INSTANCE_ATTEMPTED )); then
		capture_best_effort_ssh inspect-final inspect "${INSTANCE_NAME}"
		capture_best_effort_ssh logs-serial logs "${INSTANCE_NAME}" serial
		capture_best_effort_ssh logs-firecracker logs "${INSTANCE_NAME}" firecracker
		capture_best_effort instance-dir ls -la "${INSTANCE_DIR}"
	fi
}

cleanup_instance() {
	if (( CLEANUP_COMPLETE )) || (( ! INSTANCE_ATTEMPTED )); then
		return 0
	fi
	if [[ "${KEEP_FAILED}" == "1" ]]; then
		log "KEEP_FAILED=1; leaving ${INSTANCE_NAME} intact"
		return 0
	fi
	log "cleaning up ${INSTANCE_NAME}"
	capture_best_effort_ssh cleanup-delete delete "${INSTANCE_NAME}"
}

on_exit() {
	local rc="$1"
	if [[ "${rc}" -ne 0 ]]; then
		capture_diagnostics
		cleanup_instance
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

CONTROL_HOST="${SMOKE_SSH_HOST:-${SRV_HOSTNAME:-srv}}"
SRV_DATA_DIR="${SRV_DATA_DIR:-/var/lib/srv}"
BASE_KERNEL_PATH="${SRV_BASE_KERNEL:-}"
BASE_ROOTFS_PATH="${SRV_BASE_ROOTFS:-}"
FIRECRACKER_BIN_PATH="${SRV_FIRECRACKER_BIN:-/usr/local/bin/firecracker}"
JAILER_BASE_DIR="${SRV_JAILER_BASE_DIR:-${SRV_DATA_DIR}/jailer}"
JAILER_EXEC_NAME="$(basename "${FIRECRACKER_BIN_PATH}")"
INSTANCE_NAME="${INSTANCE_NAME:-smoke-$(date -u +%Y%m%d%H%M%S)-$$}"
ARTIFACT_DIR="${ARTIFACT_DIR:-${ARTIFACT_ROOT}/${INSTANCE_NAME}}"
INSTANCE_DIR="${SRV_DATA_DIR}/instances/${INSTANCE_NAME}"
JAILER_WORKSPACE_DIR="${JAILER_BASE_DIR}/${JAILER_EXEC_NAME}/${INSTANCE_NAME}"
READY_TIMEOUT_SECONDS="$(derive_ready_timeout_seconds)"
READY_TIMEOUT_SECONDS=$(( READY_TIMEOUT_SECONDS + READY_TIMEOUT_BUFFER_SECONDS ))

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
strict-host-assertions: ${STRICT_HOST_ASSERTIONS}
jailer-workspace-dir: ${JAILER_WORKSPACE_DIR}
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
	fail "ssh root@${CONTROL_HOST} help failed"
fi

log "creating ${INSTANCE_NAME}"
INSTANCE_ATTEMPTED=1
if ! srv_ssh_capture create new "${INSTANCE_NAME}"; then
	log "create command failed; inspecting instance state before deciding"
fi

wait_for_ready inspect-create-ready

log "instance is ready as ${TAILSCALE_NAME} (${TAILSCALE_IP})"
capture_best_effort tailscale-status-after-ready tailscale status
verify_guest_ssh guest-ssh-create-ready
assert_instance_listed list-create-ready ready
assert_strict_host_runtime create-ready

log "stopping ${INSTANCE_NAME}"
if ! srv_ssh_capture stop stop "${INSTANCE_NAME}"; then
	fail "stop ${INSTANCE_NAME} failed"
fi
if ! grep -q '^state: stopped$' "${ARTIFACT_DIR}/stop.stdout"; then
	fail "stop ${INSTANCE_NAME} did not report state: stopped"
fi
if ! srv_ssh_capture inspect-stopped inspect "${INSTANCE_NAME}"; then
	fail "inspect after stop failed"
fi
if ! grep -q '^state: stopped$' "${ARTIFACT_DIR}/inspect-stopped.stdout"; then
	fail "inspect after stop did not report state: stopped"
fi
if ! grep -q '^firecracker-pid: 0$' "${ARTIFACT_DIR}/inspect-stopped.stdout"; then
	fail "inspect after stop did not report firecracker-pid: 0"
fi
INSTANCE_TAP_DEVICE="$(extract_field "${ARTIFACT_DIR}/inspect-stopped.stdout" tap-device || true)"
assert_strict_host_cleanup stop

log "starting ${INSTANCE_NAME}"
if ! srv_ssh_capture start start "${INSTANCE_NAME}"; then
	fail "start ${INSTANCE_NAME} failed"
fi

wait_for_ready inspect-start-ready

log "instance is ready again as ${TAILSCALE_NAME} (${TAILSCALE_IP})"
capture_best_effort tailscale-status-after-restart tailscale status
verify_guest_ssh guest-ssh-start-ready
assert_instance_listed list-start-ready ready
assert_strict_host_runtime start-ready

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
if awk -v name="${INSTANCE_NAME}" '$1 == name { found = 1 } END { exit(found ? 0 : 1) }' "${ARTIFACT_DIR}/list-after-delete.stdout"; then
	fail "instance ${INSTANCE_NAME} still appears in list output after delete"
fi
if [[ -e "${INSTANCE_DIR}" ]]; then
	fail "instance directory ${INSTANCE_DIR} still exists after delete"
fi
assert_strict_host_cleanup delete

CLEANUP_COMPLETE=1

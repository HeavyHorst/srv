#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

BINARY_PATH="${BINARY_PATH:-/usr/local/bin/srv}"
HELPER_BINARY_PATH="${HELPER_BINARY_PATH:-/usr/local/bin/srv-net-helper}"
VM_RUNNER_BINARY_PATH="${VM_RUNNER_BINARY_PATH:-/usr/local/bin/srv-vm-runner}"
UNIT_PATH="${UNIT_PATH:-/etc/systemd/system/srv.service}"
HELPER_UNIT_PATH="${HELPER_UNIT_PATH:-/etc/systemd/system/srv-net-helper.service}"
VM_RUNNER_UNIT_PATH="${VM_RUNNER_UNIT_PATH:-/etc/systemd/system/srv-vm-runner.service}"
ENV_DIR="${ENV_DIR:-/etc/srv}"
ENV_PATH="${ENV_PATH:-${ENV_DIR}/srv.env}"
SERVICE_USER="${SERVICE_USER:-srv}"
SERVICE_GROUP="${SERVICE_GROUP:-srv}"
VM_RUNNER_USER="${VM_RUNNER_USER:-srv-vm}"

OVERWRITE_ENV=0
ENABLE_SERVICE=0
START_SERVICE=0

usage() {
	cat <<EOF
usage: sudo ./contrib/systemd/install.sh [options]

Installs the srv binary, systemd unit, and environment file.

Options:
  --overwrite-env  replace ${ENV_PATH} with the repo example file
  --enable         run systemctl enable srv after installation
  --start          run systemctl start srv after installation
  --enable-now     run systemctl enable --now srv after installation
  -h, --help       show this help text

Environment overrides:
  BINARY_PATH         default: ${BINARY_PATH}
  HELPER_BINARY_PATH  default: ${HELPER_BINARY_PATH}
  VM_RUNNER_BINARY_PATH default: ${VM_RUNNER_BINARY_PATH}
  UNIT_PATH           default: ${UNIT_PATH}
  HELPER_UNIT_PATH    default: ${HELPER_UNIT_PATH}
  VM_RUNNER_UNIT_PATH default: ${VM_RUNNER_UNIT_PATH}
  ENV_DIR             default: ${ENV_DIR}
  ENV_PATH            default: ${ENV_PATH}
  SERVICE_USER        default: ${SERVICE_USER}
  SERVICE_GROUP       default: ${SERVICE_GROUP}
  VM_RUNNER_USER      default: ${VM_RUNNER_USER}
EOF
}

require_root() {
	if [[ "$(id -u)" -ne 0 ]]; then
		echo "run this installer as root" >&2
		exit 1
	fi
}

require_commands() {
	local missing=()
	local cmd
	for cmd in awk go install systemctl id useradd; do
		if ! command -v "${cmd}" >/dev/null 2>&1; then
			missing+=("${cmd}")
		fi
	done
	if [[ "${#missing[@]}" -gt 0 ]]; then
		echo "missing required commands: ${missing[*]}" >&2
		exit 1
	fi
}

parse_args() {
	while [[ "$#" -gt 0 ]]; do
		case "$1" in
			--overwrite-env)
				OVERWRITE_ENV=1
				;;
			--enable)
				ENABLE_SERVICE=1
				;;
			--start)
				START_SERVICE=1
				;;
			--enable-now)
				ENABLE_SERVICE=1
				START_SERVICE=1
				;;
			-h|--help)
				usage
				exit 0
				;;
			*)
				echo "unknown option: $1" >&2
				usage >&2
				exit 1
				;;
		esac
		shift
	done
}

ensure_service_user() {
	if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
		useradd --system --user-group --home-dir /var/lib/srv --no-create-home "${SERVICE_USER}"
	fi
}

ensure_vm_runner_user() {
	if ! id -u "${VM_RUNNER_USER}" >/dev/null 2>&1; then
		useradd --system --gid "${SERVICE_GROUP}" --home-dir /var/lib/srv --no-create-home "${VM_RUNNER_USER}"
	fi
}

build_binary() {
	local target="$1"
	local output_path="$2"
	local tmp_bin
	tmp_bin="$(mktemp)"
	go build -o "${tmp_bin}" "${target}"
	install -D -m 0755 "${tmp_bin}" "${output_path}"
	rm -f "${tmp_bin}"
}

install_main_unit() {
	local rendered_unit
	rendered_unit="$(mktemp)"
	awk \
		-v binary_path="${BINARY_PATH}" \
		-v env_path="${ENV_PATH}" \
		-v service_user="${SERVICE_USER}" \
		-v service_group="${SERVICE_GROUP}" \
		'
			/^EnvironmentFile=/ { print "EnvironmentFile=" env_path; next }
			/^ExecStart=/ { print "ExecStart=" binary_path; next }
			/^User=/ { print "User=" service_user; next }
			/^Group=/ { print "Group=" service_group; next }
			{ print }
		' "${SCRIPT_DIR}/srv.service" >"${rendered_unit}"
	install -D -m 0644 "${rendered_unit}" "${UNIT_PATH}"
	rm -f "${rendered_unit}"
}

install_helper_unit() {
	local rendered_unit
	rendered_unit="$(mktemp)"
	awk \
		-v binary_path="${HELPER_BINARY_PATH}" \
		-v vm_runner_user="${VM_RUNNER_USER}" \
		-v service_group="${SERVICE_GROUP}" \
		'
			/^ExecStart=/ {
				print "ExecStart=" binary_path " -socket /run/srv/net-helper.sock -tap-user " vm_runner_user " -client-group " service_group
				next
			}
			{ print }
		' "${SCRIPT_DIR}/srv-net-helper.service" >"${rendered_unit}"
	install -D -m 0644 "${rendered_unit}" "${HELPER_UNIT_PATH}"
	rm -f "${rendered_unit}"
}

install_vm_runner_unit() {
	local rendered_unit
	rendered_unit="$(mktemp)"
	awk \
		-v binary_path="${VM_RUNNER_BINARY_PATH}" \
		-v env_path="${ENV_PATH}" \
		-v service_group="${SERVICE_GROUP}" \
		'
			/^EnvironmentFile=/ { print "EnvironmentFile=" env_path; next }
			/^ExecStart=/ { print "ExecStart=" binary_path " -socket /run/srv-vm-runner/vm-runner.sock -client-group " service_group; next }
			/^User=/ { print "User=root"; next }
			/^Group=/ { print "Group=root"; next }
			{ print }
		' "${SCRIPT_DIR}/srv-vm-runner.service" >"${rendered_unit}"
	install -D -m 0644 "${rendered_unit}" "${VM_RUNNER_UNIT_PATH}"
	rm -f "${rendered_unit}"
}

install_env() {
	install -d -m 0755 "${ENV_DIR}"
	if [[ ! -f "${ENV_PATH}" || "${OVERWRITE_ENV}" -eq 1 ]]; then
		install -m 0640 "${SCRIPT_DIR}/srv.env.example" "${ENV_PATH}"
		return
	fi
	echo "keeping existing ${ENV_PATH}" >&2
}

configured_data_dir() {
	local data_dir="/var/lib/srv"
	if [[ -f "${ENV_PATH}" ]]; then
		local configured
		configured="$(awk -F= '$1 == "SRV_DATA_DIR" { value = $2 } END { print value }' "${ENV_PATH}")"
		if [[ -n "${configured}" ]]; then
			data_dir="${configured}"
		fi
	fi
	printf '%s\n' "${data_dir}"
}

install_data_dirs() {
	local data_dir
	data_dir="$(configured_data_dir)"
	install -d -m 0755 "${data_dir}"
	install -d -m 0755 "${data_dir}/images"
	install -d -m 0755 "${data_dir}/jailer"
	install -d -m 0755 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${data_dir}/state"
	install -d -m 0770 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${data_dir}/instances"
}

reload_systemd() {
	systemctl daemon-reload
}

manage_service() {
	if (( ENABLE_SERVICE )) && (( START_SERVICE )); then
		systemctl enable --now srv
		return
	fi
	if (( ENABLE_SERVICE )); then
		systemctl enable srv
	fi
	if (( START_SERVICE )); then
		systemctl start srv
	fi
}

print_next_steps() {
	cat <<EOF
installed:
  binary:        ${BINARY_PATH}
  helper-binary: ${HELPER_BINARY_PATH}
  vm-runner-binary: ${VM_RUNNER_BINARY_PATH}
  unit:          ${UNIT_PATH}
  helper-unit:   ${HELPER_UNIT_PATH}
  vm-runner-unit: ${VM_RUNNER_UNIT_PATH}
  env:           ${ENV_PATH}

next:
  1. edit ${ENV_PATH}
  2. run: systemctl enable --now srv
  3. test: ssh root@srv help
EOF
}

main() {
	parse_args "$@"
	require_root
	require_commands
	cd "${REPO_ROOT}"
	ensure_service_user
	ensure_vm_runner_user
	build_binary ./cmd/srv "${BINARY_PATH}"
	build_binary ./cmd/srv-net-helper "${HELPER_BINARY_PATH}"
	build_binary ./cmd/srv-vm-runner "${VM_RUNNER_BINARY_PATH}"
	install_main_unit
	install_helper_unit
	install_vm_runner_unit
	install_env
	install_data_dirs
	reload_systemd
	manage_service
	print_next_steps
}

main "$@"

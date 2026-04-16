#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

BINARY_PATH="${BINARY_PATH:-/usr/local/bin/srv}"
HELPER_BINARY_PATH="${HELPER_BINARY_PATH:-/usr/local/bin/srv-net-helper}"
VM_RUNNER_BINARY_PATH="${VM_RUNNER_BINARY_PATH:-/usr/local/bin/srv-vm-runner}"
FIRECRACKER_BINARY_PATH="${FIRECRACKER_BINARY_PATH:-/usr/local/bin/firecracker}"
JAILER_BINARY_PATH="${JAILER_BINARY_PATH:-/usr/local/bin/jailer}"
FIRECRACKER_RELEASE_VERSION="${FIRECRACKER_RELEASE_VERSION:-v1.15.0}"
FIRECRACKER_RELEASE_BASE_URL="${FIRECRACKER_RELEASE_BASE_URL:-https://github.com/firecracker-microvm/firecracker/releases/download}"
INSTALL_FIRECRACKER_RELEASE="${INSTALL_FIRECRACKER_RELEASE:-1}"
UNIT_PATH="${UNIT_PATH:-/etc/systemd/system/srv.service}"
HELPER_UNIT_PATH="${HELPER_UNIT_PATH:-/etc/systemd/system/srv-net-helper.service}"
VM_RUNNER_UNIT_PATH="${VM_RUNNER_UNIT_PATH:-/etc/systemd/system/srv-vm-runner.service}"
ENV_DIR="${ENV_DIR:-/etc/srv}"
ENV_PATH="${ENV_PATH:-${ENV_DIR}/srv.env}"
SERVICE_USER="${SERVICE_USER:-srv}"
SERVICE_GROUP="${SERVICE_GROUP:-srv}"
VM_RUNNER_USER="${VM_RUNNER_USER:-srv-vm}"

SRV_VERSION="${SRV_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

OVERWRITE_ENV=0
ENABLE_SERVICE=0
START_SERVICE=0

usage() {
	cat <<EOF
usage: sudo ./contrib/systemd/install.sh [options]

Installs the srv binaries, the systemd units, the environment file, and an
official static Firecracker/jailer release pair by default.

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
  FIRECRACKER_BINARY_PATH default: ${FIRECRACKER_BINARY_PATH}
  JAILER_BINARY_PATH  default: ${JAILER_BINARY_PATH}
  FIRECRACKER_RELEASE_VERSION default: ${FIRECRACKER_RELEASE_VERSION}
  FIRECRACKER_RELEASE_BASE_URL default: ${FIRECRACKER_RELEASE_BASE_URL}
  INSTALL_FIRECRACKER_RELEASE default: ${INSTALL_FIRECRACKER_RELEASE}
  UNIT_PATH           default: ${UNIT_PATH}
  HELPER_UNIT_PATH    default: ${HELPER_UNIT_PATH}
  VM_RUNNER_UNIT_PATH default: ${VM_RUNNER_UNIT_PATH}
  ENV_DIR             default: ${ENV_DIR}
  ENV_PATH            default: ${ENV_PATH}
  SERVICE_USER        default: ${SERVICE_USER}
  SERVICE_GROUP       default: ${SERVICE_GROUP}
  VM_RUNNER_USER      default: ${VM_RUNNER_USER}
  SRV_VERSION         default: git describe or "dev"
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
	for cmd in awk chown curl go install sha256sum systemctl tar id useradd; do
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
	go build -ldflags "-X srv/internal/version.Version=${SRV_VERSION}" -o "${tmp_bin}" "${target}"
	install -D -m 0755 "${tmp_bin}" "${output_path}"
	rm -f "${tmp_bin}"
}

firecracker_release_arch() {
	case "$(uname -m)" in
		x86_64|aarch64)
			printf '%s\n' "$(uname -m)"
			;;
		*)
			echo "unsupported architecture for Firecracker release download: $(uname -m)" >&2
			exit 1
			;;
	esac
}

install_firecracker_release() {
	if [[ "${INSTALL_FIRECRACKER_RELEASE}" != "1" ]]; then
		return
	fi

	local arch version archive_name download_url checksum_url tmp_dir archive_path checksum_path release_dir
	arch="$(firecracker_release_arch)"
	version="${FIRECRACKER_RELEASE_VERSION}"
	archive_name="firecracker-${version}-${arch}.tgz"
	download_url="${FIRECRACKER_RELEASE_BASE_URL}/${version}/${archive_name}"
	checksum_url="${download_url}.sha256.txt"
	tmp_dir="$(mktemp -d)"
	archive_path="${tmp_dir}/${archive_name}"
	checksum_path="${archive_path}.sha256.txt"

	curl -fsSL -o "${archive_path}" "${download_url}"
	curl -fsSL -o "${checksum_path}" "${checksum_url}"
	(
		cd "${tmp_dir}"
		sha256sum -c "$(basename "${checksum_path}")"
	)
	tar -xzf "${archive_path}" -C "${tmp_dir}"
	release_dir="${tmp_dir}/release-${version}-${arch}"
	install -D -m 0755 "${release_dir}/firecracker-${version}-${arch}" "${FIRECRACKER_BINARY_PATH}"
	install -D -m 0755 "${release_dir}/jailer-${version}-${arch}" "${JAILER_BINARY_PATH}"
	rm -rf "${tmp_dir}"
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
			/^Group=/ { print "Group=" service_group; next }
			{ print }
		' "${SCRIPT_DIR}/srv-vm-runner.service" >"${rendered_unit}"
	install -D -m 0644 "${rendered_unit}" "${VM_RUNNER_UNIT_PATH}"
	rm -f "${rendered_unit}"
}

render_env_file() {
	local destination="$1"
	local rendered_env
	rendered_env="$(mktemp)"
	awk \
		-v firecracker_binary_path="${FIRECRACKER_BINARY_PATH}" \
		-v jailer_binary_path="${JAILER_BINARY_PATH}" \
		'
			/^SRV_FIRECRACKER_BIN=/ { print "SRV_FIRECRACKER_BIN=" firecracker_binary_path; next }
			/^SRV_JAILER_BIN=/ { print "SRV_JAILER_BIN=" jailer_binary_path; next }
			{ print }
		' "${SCRIPT_DIR}/srv.env.example" >"${rendered_env}"
	install -m 0640 "${rendered_env}" "${destination}"
	rm -f "${rendered_env}"
}

read_env_value() {
	local key="$1"
	awk -F= -v key="${key}" '$1 == key { print substr($0, index($0, "=") + 1); exit }' "${ENV_PATH}"
}

upsert_env_value() {
	local key="$1"
	local value="$2"
	local rendered_env
	rendered_env="$(mktemp)"
	awk \
		-v key="${key}" \
		-v value="${value}" \
		'
			BEGIN { updated = 0 }
			$0 ~ ("^" key "=") { print key "=" value; updated = 1; next }
			{ print }
			END {
				if (!updated) {
					print key "=" value
				}
			}
		' "${ENV_PATH}" >"${rendered_env}"
	install -m 0640 "${rendered_env}" "${ENV_PATH}"
	rm -f "${rendered_env}"
}

reconcile_env_binary_path() {
	local key="$1"
	local desired_value="$2"
	shift 2
	local current_value
	current_value="$(read_env_value "${key}" || true)"
	if [[ -z "${current_value}" ]]; then
		upsert_env_value "${key}" "${desired_value}"
		return
	fi
	local allowed_value
	for allowed_value in "$@"; do
		if [[ "${current_value}" == "${allowed_value}" ]]; then
			upsert_env_value "${key}" "${desired_value}"
			return
		fi
	done
	echo "keeping existing ${key}=${current_value}" >&2
}

reconcile_env_binary_paths() {
	reconcile_env_binary_path "SRV_FIRECRACKER_BIN" "${FIRECRACKER_BINARY_PATH}" "/usr/bin/firecracker" "/usr/local/bin/firecracker"
	reconcile_env_binary_path "SRV_JAILER_BIN" "${JAILER_BINARY_PATH}" "/usr/bin/jailer" "/usr/local/bin/jailer"
}

install_env() {
	install -d -m 0755 "${ENV_DIR}"
	if [[ ! -f "${ENV_PATH}" || "${OVERWRITE_ENV}" -eq 1 ]]; then
		render_env_file "${ENV_PATH}"
		return
	fi
	echo "keeping existing ${ENV_PATH}" >&2
	if [[ "${INSTALL_FIRECRACKER_RELEASE}" == "1" ]]; then
		reconcile_env_binary_paths
	fi
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
	install -d -m 0770 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${data_dir}/backups"
	install -d -m 0770 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${data_dir}/instances"
	chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "${data_dir}/state" "${data_dir}/backups" "${data_dir}/instances"
}

reload_systemd() {
	systemctl daemon-reload
}

restart_service_stack() {
	systemctl stop srv srv-net-helper srv-vm-runner
	# systemd 260 can trip over delegated cgroup reuse when srv-vm-runner is
	# started immediately after stop; give the subtree time to disappear even if
	# the previous unit state was failed instead of active.
	sleep 5
	systemctl start srv-vm-runner srv-net-helper srv
}

manage_service() {
	if (( ENABLE_SERVICE )) && (( START_SERVICE )); then
		systemctl enable srv
		restart_service_stack
		return
	fi
	if (( ENABLE_SERVICE )); then
		systemctl enable srv
	fi
	if (( START_SERVICE )); then
		restart_service_stack
	fi
}

print_next_steps() {
	cat <<EOF
installed:
  binary:        ${BINARY_PATH}
  helper-binary: ${HELPER_BINARY_PATH}
  vm-runner-binary: ${VM_RUNNER_BINARY_PATH}
  firecracker-binary: ${FIRECRACKER_BINARY_PATH}
  jailer-binary: ${JAILER_BINARY_PATH}
  unit:          ${UNIT_PATH}
  helper-unit:   ${HELPER_UNIT_PATH}
  vm-runner-unit: ${VM_RUNNER_UNIT_PATH}
  env:           ${ENV_PATH}

next:
  1. edit ${ENV_PATH}
  2. enable IPv4 forwarding for guest NAT, for example:
     tee /etc/sysctl.d/90-srv-ip-forward.conf >/dev/null <<'EOT'
     net.ipv4.ip_forward = 1
     EOT
     sysctl --system
  3. run: systemctl enable --now srv
  4. test: ssh srv help
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
	install_firecracker_release
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

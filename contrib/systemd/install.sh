#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

BINARY_PATH="${BINARY_PATH:-/usr/local/bin/srv}"
UNIT_PATH="${UNIT_PATH:-/etc/systemd/system/srv.service}"
ENV_DIR="${ENV_DIR:-/etc/srv}"
ENV_PATH="${ENV_PATH:-${ENV_DIR}/srv.env}"

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
  BINARY_PATH   default: ${BINARY_PATH}
  UNIT_PATH     default: ${UNIT_PATH}
  ENV_DIR       default: ${ENV_DIR}
  ENV_PATH      default: ${ENV_PATH}
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
	for cmd in awk go install systemctl; do
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

build_binary() {
	local tmp_bin
	tmp_bin="$(mktemp)"
	go build -o "${tmp_bin}" ./cmd/srv
	install -D -m 0755 "${tmp_bin}" "${BINARY_PATH}"
	rm -f "${tmp_bin}"
}

install_unit() {
	local rendered_unit
	rendered_unit="$(mktemp)"
	awk \
		-v binary_path="${BINARY_PATH}" \
		-v env_path="${ENV_PATH}" \
		'
			/^EnvironmentFile=/ { print "EnvironmentFile=" env_path; next }
			/^ExecStart=/ { print "ExecStart=" binary_path; next }
			{ print }
		' "${SCRIPT_DIR}/srv.service" >"${rendered_unit}"
	install -D -m 0644 "${rendered_unit}" "${UNIT_PATH}"
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
  binary: ${BINARY_PATH}
  unit:   ${UNIT_PATH}
  env:    ${ENV_PATH}

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
	build_binary
	install_unit
	install_env
	reload_systemd
	manage_service
	print_next_steps
}

main "$@"

#!/usr/bin/env bash
set -euo pipefail

STATE_DIR=/var/lib/srv
DONE_FILE="${STATE_DIR}/bootstrap.done"
METADATA_URL="http://169.254.169.254/"
METADATA_FILE="$(mktemp /run/srv-bootstrap.XXXXXX.json)"
OPENCODE_CONFIG_DIR=/root/.config/opencode
OPENCODE_CONFIG_PATH="${OPENCODE_CONFIG_DIR}/opencode.json"
PI_CONFIG_DIR=/root/.pi/agent
PI_AUTH_PATH="${PI_CONFIG_DIR}/auth.json"
PI_MODELS_PATH="${PI_CONFIG_DIR}/models.json"
PI_SETTINGS_PATH="${PI_CONFIG_DIR}/settings.json"
trap 'rm -f "${METADATA_FILE}"' EXIT

log() {
	echo "srv-bootstrap: $*" >&2
}

primary_iface() {
	ip -4 route list default | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit }}'
}

fallback_iface() {
	ip -o link show | awk -F': ' '$2 != "lo" && $2 !~ /^tailscale/ { print $2; exit }'
}

default_gateway_ip() {
	ip -4 route list default | awk '{for (i = 1; i <= NF; i++) if ($i == "via") { print $(i + 1); exit }}'
}

file_matches_expected_content() {
	local path="$1"
	local expected="$2"
	[[ -f "${path}" ]] || return 1
	diff -q "${path}" <(printf '%s' "${expected}") >/dev/null 2>&1
}

opencode_config_is_managed() {
	[[ -f "${OPENCODE_CONFIG_PATH}" ]] && grep -Fq '"apiKey": "srv-zen-gateway"' "${OPENCODE_CONFIG_PATH}"
}

remove_opencode_config() {
	if opencode_config_is_managed; then
		rm -f "${OPENCODE_CONFIG_PATH}"
	fi
}

write_opencode_config() {
	local gateway_ip="$1"
	local gateway_port="$2"
	if [[ -f "${OPENCODE_CONFIG_PATH}" ]] && ! opencode_config_is_managed; then
		log "leaving existing OpenCode config in place"
		return 1
	fi

	install -d -m 0700 "${OPENCODE_CONFIG_DIR}"
	# OpenCode's built-in "opencode" provider hides paid Zen models unless it sees
	# a non-empty apiKey locally. This placeholder never reaches the real upstream
	# because the host-side srv gateway overwrites auth before proxying.
	cat >"${OPENCODE_CONFIG_PATH}" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "provider": {
    "opencode": {
      "options": {
        "baseURL": "http://${gateway_ip}:${gateway_port}/v1",
        "apiKey": "srv-zen-gateway"
      }
    }
  }
}
EOF
	chmod 0600 "${OPENCODE_CONFIG_PATH}"
	return 0
}

pi_auth_config_is_managed() {
	[[ -f "${PI_AUTH_PATH}" ]] && grep -Fq '"opencode"' "${PI_AUTH_PATH}" && grep -Fq '"key": "srv-zen-gateway"' "${PI_AUTH_PATH}"
}

pi_models_config_is_managed() {
	[[ -f "${PI_MODELS_PATH}" ]] && grep -Fq '"opencode"' "${PI_MODELS_PATH}" && grep -Fq '"apiKey": "srv-zen-gateway"' "${PI_MODELS_PATH}"
}

managed_pi_settings_content() {
	cat <<'EOF'
{
  "defaultProvider": "opencode"
}
EOF
}

pi_settings_config_is_managed() {
	local expected
	expected="$(managed_pi_settings_content)"
	file_matches_expected_content "${PI_SETTINGS_PATH}" "${expected}"
}

remove_pi_config() {
	if pi_auth_config_is_managed; then
		rm -f "${PI_AUTH_PATH}"
	fi
	if pi_models_config_is_managed; then
		rm -f "${PI_MODELS_PATH}"
	fi
	if pi_settings_config_is_managed; then
		rm -f "${PI_SETTINGS_PATH}"
	fi
	rmdir --ignore-fail-on-non-empty "${PI_CONFIG_DIR}" 2>/dev/null || true
}

write_pi_auth_config() {
	if [[ -f "${PI_AUTH_PATH}" ]] && ! pi_auth_config_is_managed; then
		log "leaving existing Pi auth config in place"
		return 1
	fi
	cat >"${PI_AUTH_PATH}" <<'EOF'
{
  "opencode": {
    "type": "api_key",
    "key": "srv-zen-gateway"
  }
}
EOF
	chmod 0600 "${PI_AUTH_PATH}"
	return 0
}

write_pi_models_config() {
	local gateway_ip="$1"
	local gateway_port="$2"
	if [[ -f "${PI_MODELS_PATH}" ]] && ! pi_models_config_is_managed; then
		log "leaving existing Pi models config in place"
		return 1
	fi
	cat >"${PI_MODELS_PATH}" <<EOF
{
  "providers": {
    "opencode": {
      "baseUrl": "http://${gateway_ip}:${gateway_port}/v1",
      "apiKey": "srv-zen-gateway"
    }
  }
}
EOF
	chmod 0600 "${PI_MODELS_PATH}"
	return 0
}

write_pi_settings_config() {
	local expected
	expected="$(managed_pi_settings_content)"
	if [[ -f "${PI_SETTINGS_PATH}" ]] && ! pi_settings_config_is_managed; then
		log "leaving existing Pi settings in place"
		return 1
	fi
	printf '%s' "${expected}" >"${PI_SETTINGS_PATH}"
	chmod 0600 "${PI_SETTINGS_PATH}"
	return 0
}

write_pi_config() {
	local gateway_ip="$1"
	local gateway_port="$2"
	local wrote_any=1

	install -d -m 0700 "${PI_CONFIG_DIR}"
	if write_pi_auth_config; then
		wrote_any=0
	fi
	if write_pi_models_config "${gateway_ip}" "${gateway_port}"; then
		wrote_any=0
	fi
	if write_pi_settings_config; then
		wrote_any=0
	fi
	return "${wrote_any}"
}

mkdir -p "${STATE_DIR}"
ln -sf /proc/net/pnp /etc/resolv.conf

iface="$(primary_iface || true)"
if [[ -z "${iface}" ]]; then
	iface="$(fallback_iface || true)"
fi
if [[ -z "${iface}" ]]; then
	echo "srv-bootstrap: unable to determine primary guest interface" >&2
	exit 1
fi

# Firecracker MMDS lives on a link-local address, so add a direct route before fetching metadata.
ip route replace 169.254.169.254/32 dev "${iface}"
# Firecracker MMDS serves IMDS/plain-text by default unless the guest asks for JSON.
curl --fail --silent --show-error --retry 30 --retry-delay 1 --retry-all-errors \
	-H 'Accept: application/json' \
	"${METADATA_URL}" >"${METADATA_FILE}"

version="$(jq -er '.srv.version' "${METADATA_FILE}")"
if [[ "${version}" != "1" ]]; then
	echo "srv-bootstrap: unsupported metadata version ${version}" >&2
	exit 1
fi

hostname_value="$(jq -er '.srv.hostname' "${METADATA_FILE}")"
auth_key="$(jq -r '.srv.tailscale_auth_key // empty' "${METADATA_FILE}")"
control_url="$(jq -r '.srv.tailscale_control_url // empty' "${METADATA_FILE}")"
zen_gateway_port="$(jq -r '.srv.zen_gateway_port // empty' "${METADATA_FILE}")"
mapfile -t tags < <(jq -r '.srv.tailscale_tags[]? // empty' "${METADATA_FILE}")
tag_csv="$(IFS=,; echo "${tags[*]}")"
log "starting bootstrap for ${hostname_value} via ${iface}"

if [[ -n "${zen_gateway_port}" ]]; then
	zen_gateway_host="$(default_gateway_ip || true)"
	if [[ -z "${zen_gateway_host}" ]]; then
		remove_opencode_config
		remove_pi_config
		log "skipping Zen gateway guest config because the default gateway IP is unavailable"
	else
		if write_opencode_config "${zen_gateway_host}" "${zen_gateway_port}"; then
			log "configured OpenCode to use http://${zen_gateway_host}:${zen_gateway_port}/v1"
		fi
		if write_pi_config "${zen_gateway_host}" "${zen_gateway_port}"; then
			log "configured Pi to use http://${zen_gateway_host}:${zen_gateway_port}/v1"
		fi
	fi
else
	remove_opencode_config
	remove_pi_config
fi

# The cloned rootfs may carry restrictive ownership from the image build; keep
# bootstrapping moving even if we can only set the transient hostname.
hostnamectl set-hostname --transient "${hostname_value}"
systemctl start tailscaled.service

if [[ -n "${auth_key}" ]]; then
	log "joining the tailnet with tailscale up"
	tailscale_args=(
		up
		--hostname="${hostname_value}"
		--ssh
		--timeout=30s
	)
	tailscale_args+=(--auth-key="${auth_key}")
	if [[ -n "${control_url}" ]]; then
		tailscale_args+=(--login-server="${control_url}")
	fi
	if [[ -n "${tag_csv}" ]]; then
		tailscale_args+=(--advertise-tags="${tag_csv}")
	fi
	tailscale "${tailscale_args[@]}"
fi

date --iso-8601=seconds >"${DONE_FILE}"
chmod 0600 "${DONE_FILE}"
log "bootstrap completed successfully"
unset auth_key

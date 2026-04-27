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
MANAGED_GATEWAY_PLACEHOLDER="srv-provider-gateway"
LEGACY_MANAGED_GATEWAY_PLACEHOLDER="srv-zen-gateway"
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
	[[ -f "${OPENCODE_CONFIG_PATH}" ]] && \
		(grep -Fq "\"apiKey\": \"${MANAGED_GATEWAY_PLACEHOLDER}\"" "${OPENCODE_CONFIG_PATH}" || \
			grep -Fq "\"apiKey\": \"${LEGACY_MANAGED_GATEWAY_PLACEHOLDER}\"" "${OPENCODE_CONFIG_PATH}")
}

remove_opencode_config() {
	if opencode_config_is_managed; then
		rm -f "${OPENCODE_CONFIG_PATH}"
	fi
}

write_opencode_config() {
	local gateway_ip="$1"
	local zen_port="$2"
	local deepseek_port="$3"
	if [[ -f "${OPENCODE_CONFIG_PATH}" ]] && ! opencode_config_is_managed; then
		log "leaving existing OpenCode config in place"
		return 1
	fi

	install -d -m 0700 "${OPENCODE_CONFIG_DIR}"
	local config_json
	config_json="$(jq -n \
		--arg schema "https://opencode.ai/config.json" \
		--arg zen_url "http://${gateway_ip}:${zen_port}/v1" \
		--arg deepseek_url "http://${gateway_ip}:${deepseek_port}/v1" \
		--arg zen_port "${zen_port}" \
		--arg deepseek_port "${deepseek_port}" \
		--arg gateway_key "${MANAGED_GATEWAY_PLACEHOLDER}" \
		'{
			"\u0024schema": $schema,
			"provider": {}
		}
		| if $zen_port != "" and ($zen_port|test("^[1-9]")) then
			.provider.opencode.options.baseURL = $zen_url |
			.provider.opencode.options.apiKey = $gateway_key
		  else . end
		| if $deepseek_port != "" and ($deepseek_port|test("^[1-9]")) then
			.provider.deepseek.options.baseURL = $deepseek_url |
			.provider.deepseek.options.apiKey = $gateway_key
		  else . end')"
	printf '%s\n' "${config_json}" >"${OPENCODE_CONFIG_PATH}"
	chmod 0600 "${OPENCODE_CONFIG_PATH}"
	return 0
}

pi_auth_config_is_managed() {
	[[ -f "${PI_AUTH_PATH}" ]] && jq -e \
		--arg gateway_key "${MANAGED_GATEWAY_PLACEHOLDER}" \
		--arg legacy_gateway_key "${LEGACY_MANAGED_GATEWAY_PLACEHOLDER}" \
		'(.opencode.key == $gateway_key) or (.deepseek.key == $gateway_key) or (.opencode.key == $legacy_gateway_key) or (.deepseek.key == $legacy_gateway_key)' \
		"${PI_AUTH_PATH}" >/dev/null 2>&1
}

pi_models_config_is_managed() {
	[[ -f "${PI_MODELS_PATH}" ]] && jq -e \
		--arg gateway_key "${MANAGED_GATEWAY_PLACEHOLDER}" \
		--arg legacy_gateway_key "${LEGACY_MANAGED_GATEWAY_PLACEHOLDER}" \
		'(.providers.opencode.apiKey == $gateway_key) or (.providers.deepseek.apiKey == $gateway_key) or (.providers.opencode.apiKey == $legacy_gateway_key) or (.providers.deepseek.apiKey == $legacy_gateway_key)' \
		"${PI_MODELS_PATH}" >/dev/null 2>&1
}

managed_pi_settings_content() {
	local default_provider="$1"
	cat <<EOF
{
  "defaultProvider": "${default_provider}"
}
EOF
}

pi_settings_default_provider() {
	local zen_port="$1"
	local deepseek_port="$2"
	if [[ -n "${zen_port}" ]] && [[ "${zen_port}" =~ ^[1-9] ]]; then
		echo "opencode"
		return 0
	fi
	if [[ -n "${deepseek_port}" ]] && [[ "${deepseek_port}" =~ ^[1-9] ]]; then
		echo "deepseek"
		return 0
	fi
	echo "opencode"
}

pi_settings_config_is_managed() {
	local default_provider
	local expected
	for default_provider in opencode deepseek; do
		expected="$(managed_pi_settings_content "${default_provider}")"
		if file_matches_expected_content "${PI_SETTINGS_PATH}" "${expected}"; then
			return 0
		fi
	done
	return 1
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
	local zen_port="$1"
	local deepseek_port="$2"
	if [[ -f "${PI_AUTH_PATH}" ]] && ! pi_auth_config_is_managed; then
		log "leaving existing Pi auth config in place"
		return 1
	fi
	local auth_json
	auth_json="$(jq -n \
		--arg zen_port "${zen_port}" \
		--arg deepseek_port "${deepseek_port}" \
		--arg gateway_key "${MANAGED_GATEWAY_PLACEHOLDER}" \
		'{}
		| if $zen_port != "" and ($zen_port|test("^[1-9]")) then
			.opencode.type = "api_key" |
			.opencode.key = $gateway_key
		  else . end
		| if $deepseek_port != "" and ($deepseek_port|test("^[1-9]")) then
			.deepseek.type = "api_key" |
			.deepseek.key = $gateway_key
		  else . end')"
	printf '%s\n' "${auth_json}" >"${PI_AUTH_PATH}"
	chmod 0600 "${PI_AUTH_PATH}"
	return 0
}

write_pi_models_config() {
	local gateway_ip="$1"
	local zen_port="$2"
	local deepseek_port="$3"
	if [[ -f "${PI_MODELS_PATH}" ]] && ! pi_models_config_is_managed; then
		log "leaving existing Pi models config in place"
		return 1
	fi
	local models_json
	models_json="$(jq -n \
		--arg zen_url "http://${gateway_ip}:${zen_port}/v1" \
		--arg deepseek_url "http://${gateway_ip}:${deepseek_port}/v1" \
		--arg zen_port "${zen_port}" \
		--arg deepseek_port "${deepseek_port}" \
		--arg gateway_key "${MANAGED_GATEWAY_PLACEHOLDER}" \
		'{ "providers": {} }
		| if $zen_port != "" and ($zen_port|test("^[1-9]")) then
			.providers.opencode.baseUrl = $zen_url |
			.providers.opencode.apiKey = $gateway_key
		  else . end
		| if $deepseek_port != "" and ($deepseek_port|test("^[1-9]")) then
			.providers.deepseek.baseUrl = $deepseek_url |
			.providers.deepseek.apiKey = $gateway_key |
			.providers.deepseek.compat.supportsDeveloperRole = false |
			.providers.deepseek.compat.supportsStore = false
		  else . end')"
	printf '%s\n' "${models_json}" >"${PI_MODELS_PATH}"
	chmod 0600 "${PI_MODELS_PATH}"
	return 0
}

write_pi_settings_config() {
	local zen_port="$1"
	local deepseek_port="$2"
	local expected
	expected="$(managed_pi_settings_content "$(pi_settings_default_provider "${zen_port}" "${deepseek_port}")")"
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
	local zen_port="$2"
	local deepseek_port="$3"
	local wrote_any=1

	install -d -m 0700 "${PI_CONFIG_DIR}"
	if write_pi_auth_config "${zen_port}" "${deepseek_port}"; then
		wrote_any=0
	fi
	if write_pi_models_config "${gateway_ip}" "${zen_port}" "${deepseek_port}"; then
		wrote_any=0
	fi
	if write_pi_settings_config "${zen_port}" "${deepseek_port}"; then
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
deepseek_gateway_port="$(jq -r '.srv.deepseek_gateway_port // empty' "${METADATA_FILE}")"
mapfile -t tags < <(jq -r '.srv.tailscale_tags[]? // empty' "${METADATA_FILE}")
tag_csv="$(IFS=,; echo "${tags[*]}")"
log "starting bootstrap for ${hostname_value} via ${iface}"

zen_enabled=0
if [[ -n "${zen_gateway_port}" ]] && [[ "${zen_gateway_port}" =~ ^[1-9] ]]; then
	zen_enabled=1
fi
deepseek_enabled=0
if [[ -n "${deepseek_gateway_port}" ]] && [[ "${deepseek_gateway_port}" =~ ^[1-9] ]]; then
	deepseek_enabled=1
fi

if [[ "${zen_enabled}" -eq 1 ]] || [[ "${deepseek_enabled}" -eq 1 ]]; then
	gateway_host="$(default_gateway_ip || true)"
	if [[ -z "${gateway_host}" ]]; then
		remove_opencode_config
		remove_pi_config
		log "skipping gateway guest config because the default gateway IP is unavailable"
	else
		if write_opencode_config "${gateway_host}" "${zen_gateway_port}" "${deepseek_gateway_port}"; then
			log "configured OpenCode"
		fi
		if write_pi_config "${gateway_host}" "${zen_gateway_port}" "${deepseek_gateway_port}"; then
			log "configured Pi"
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

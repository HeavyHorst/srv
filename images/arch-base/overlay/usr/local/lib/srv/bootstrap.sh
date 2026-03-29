#!/usr/bin/env bash
set -euo pipefail

STATE_DIR=/var/lib/srv
DONE_FILE="${STATE_DIR}/bootstrap.done"
METADATA_URL="http://169.254.169.254/"
METADATA_FILE="$(mktemp /run/srv-bootstrap.XXXXXX.json)"
trap 'rm -f "${METADATA_FILE}"' EXIT

primary_iface() {
	ip -4 route list default | awk '{for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit }}'
}

fallback_iface() {
	ip -o link show | awk -F': ' '$2 != "lo" && $2 !~ /^tailscale/ { print $2; exit }'
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
auth_key="$(jq -er '.srv.tailscale_auth_key' "${METADATA_FILE}")"
control_url="$(jq -r '.srv.tailscale_control_url // empty' "${METADATA_FILE}")"
mapfile -t tags < <(jq -r '.srv.tailscale_tags[]? // empty' "${METADATA_FILE}")

# The cloned rootfs may carry restrictive ownership from the image build; keep
# bootstrapping moving even if we can only set the transient hostname.
hostnamectl set-hostname --transient "${hostname_value}"
systemctl start tailscaled.service

tailscale_args=(
	up
	--auth-key="${auth_key}"
	--hostname="${hostname_value}"
	--ssh
	--timeout=30s
)

if [[ -n "${control_url}" ]]; then
	tailscale_args+=(--login-server="${control_url}")
fi
if [[ "${#tags[@]}" -gt 0 ]]; then
	tailscale_args+=(--advertise-tags="$(IFS=,; echo "${tags[*]}")")
fi

tailscale "${tailscale_args[@]}"

date --iso-8601=seconds >"${DONE_FILE}"
chmod 0600 "${DONE_FILE}"
unset auth_key

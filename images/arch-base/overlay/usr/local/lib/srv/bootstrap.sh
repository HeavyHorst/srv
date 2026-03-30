#!/usr/bin/env bash
set -euo pipefail

STATE_DIR=/var/lib/srv
DONE_FILE="${STATE_DIR}/bootstrap.done"
METADATA_URL="http://169.254.169.254/"
METADATA_FILE="$(mktemp /run/srv-bootstrap.XXXXXX.json)"
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
mapfile -t tags < <(jq -r '.srv.tailscale_tags[]? // empty' "${METADATA_FILE}")
tag_csv="$(IFS=,; echo "${tags[*]}")"
log "starting bootstrap for ${hostname_value} via ${iface}"

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
